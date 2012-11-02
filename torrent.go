package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"time"
	"strings"
	"github.com/nictuku/dht"
	"github.com/nictuku/nettools"
	"github.com/ghais/gocache"
)


type Config struct{
	MAX_NUM_PEERS 				int 
	TARGET_NUM_PEERS 			int 
	MAX_DOWNLOADING_CONNECTION  int	
	MAX_UPLOADING_CONNECTION	int	
	MAX_OUR_REQUESTS 			int
	MAX_PEER_REQUESTS 			int 
}

// BitTorrent message types. Sources:
// http://bittorrent.org/beps/bep_0003.html
// http://wiki.theory.org/BitTorrentSpecification
const (
	CHOKE = iota
	UNCHOKE
	INTERESTED
	NOT_INTERESTED
	HAVE
	BITFIELD
	REQUEST
	PIECE
	CANCEL
	PORT // Not implemented. For DHT support.
	MODE_READ
	MODE_WRITE
)

// Should be overriden by flag. Not thread safe.
var port int
var useUPnP bool
var fileDir string
var useDHT bool
var trackerLessMode bool

var cfg Config

func init() {
	flag.StringVar(&fileDir, "fileDir", ".", "path to directory where files are stored")
	// If the port is 0, picks up a random port - but the DHT will keep
	// running on port 0 because ListenUDP doesn't do that.
	// Don't use port 6881 which blacklisted by some trackers.
	flag.IntVar(&port, "port", 7777, "Port to listen on.")
	flag.BoolVar(&useUPnP, "useUPnP", false, "Use UPnP to open port in firewall.")
	flag.BoolVar(&useDHT, "useDHT", false, "Use DHT to get peers.")
	flag.BoolVar(&trackerLessMode, "trackerLessMode", false, "Do not get peers from the tracker. Good for "+
		"testing the DHT mode.")
	rand.Seed(int64(time.Now().Nanosecond()))

	cfg = Config{MAX_NUM_PEERS : 60, TARGET_NUM_PEERS : 5, MAX_DOWNLOADING_CONNECTION : 1,
		MAX_UPLOADING_CONNECTION : 1, MAX_OUR_REQUESTS : 10, MAX_PEER_REQUESTS : 10}
}



func chooseListenPort() (listenPort int, err error) {
	listenPort = port
	if useUPnP {
		log.Println("Using UPnP to open port.")
		// TODO: Look for ports currently in use. Handle collisions.
		var nat NAT
		nat, err = Discover()
		if err != nil {
			log.Println("Unable to discover NAT:", err)
			return
		}
		// TODO: Check if the port is already mapped by someone else.
		err2 := nat.DeletePortMapping("TCP", listenPort)
		if err2 != nil {
			log.Println("Unable to delete port mapping", err2)
		}
		err = nat.AddPortMapping("TCP", listenPort, listenPort,
			"Taipei-Torrent port "+strconv.Itoa(listenPort), 0)
		if err != nil {
			log.Println("Unable to forward listen port", err)
			return
		}
	}
	return
}

func (t *TorrentSession) listenForPeerConnections(conChan chan net.Conn) {
	listenString := ":" + strconv.Itoa(t.si.Port)
	
	log.Println("listen on ", listenString)

	listener, err := net.Listen("tcp", listenString)
	if err != nil {
		log.Fatal("Listen failed:", err)
	}

	// If port was not set by UPnP, and was 0, get the actual port
	if t.si.Port == 0 {
		// so we can send it to trackers.
		_, p, err := net.SplitHostPort(listener.Addr().String())
		if err == nil {
			t.si.Port, err = strconv.Atoi(p)
		}

		if err != nil {
			log.Fatal("net.Listen() gave us an invalid port: ", err)
		}
	}

	log.Println("Listening for peers on port:", t.si.Port)
	go func() {
		for {
			var conn net.Conn
			conn, err = listener.Accept()
			if err != nil {
				log.Println("Listener failed:", err)
			} else {
				// log.Println("A peer contacted us", conn.RemoteAddr().String())
				conChan <- conn
			}
		}
	}()
}


type TorrentSession struct {
	m               *MetaInfo
	si              *SessionInfo
	ti              *TrackerResponse
	fileStore       FileStore
	trackerInfoChan chan *TrackerResponse
	peers           map[string]*peerState
	peerMessageChan chan peerMessage
	pieceSet        *Bitset // The pieces we have
	totalPieces     int
	totalSize       int64
	lastPieceLength int
	goodPieces      int
	activePieces    map[int]*ActivePiece
	lastHeartBeat   time.Time
	dht             *dht.DHTEngine
	history			map[string]*DownloadUpload
	ioRequestChan 	chan *IoArgs
	ioResponceChan 	chan interface{}
	cache			*cache.LRUCache
}


func NewTorrentSession(torrent string) (ts *TorrentSession, err error) {

	var listenPort int
	if listenPort, err = chooseListenPort(); err != nil {
		log.Println("Could not choose listen port.")
		log.Println("Peer connectivity will be affected.")
	}

	t := &TorrentSession{peers: make(map[string]*peerState),
		peerMessageChan: make(chan peerMessage, 100),
		activePieces:    make(map[int]*ActivePiece),
		history:		make(map[string]*DownloadUpload),
		ioRequestChan:	make(chan *IoArgs, 100),
		ioResponceChan: make(chan interface{}, 100),
		}

	t.cache = cache.NewLRUCache(10 * 1024 *1024)	

	t.m, err = getMetaInfo(torrent)
	if err != nil {
		return
	}
	log.Printf("Tracker: %v, Comment: %v, InfoHash: %x, Encoding: %v, Private: %v",
		t.m.Announce, t.m.Comment, t.m.InfoHash, t.m.Encoding, t.m.Info.Private)
	if e := t.m.Encoding; e != "" && e != "UTF-8" {
		return nil, errors.New(fmt.Sprintf("Unknown encoding %s", e))
	}
	ext := ".torrent"
	dir := fileDir
	if len(t.m.Info.Files) != 0 {
		dir += "/" + filepath.Base(torrent)
		if dir[len(dir)-len(ext):] == ext {
			dir = dir[:len(dir)-len(ext)]
		}
	}

	t.fileStore, t.totalSize, err = NewFileStore(&t.m.Info, dir)
	if err != nil {
		return
	}
	t.lastPieceLength = int(t.totalSize % t.m.Info.PieceLength)

	start := time.Now()
	good, bad, pieceSet, err := checkPieces(t.fileStore, t.totalSize, t.m)
	end := time.Now()
	log.Printf("Computed missing pieces (%.2f seconds)", end.Sub(start).Seconds())
	if err != nil {
		return
	}
	t.pieceSet = pieceSet
	t.totalPieces = good + bad
	t.goodPieces = good
	log.Println("Good pieces:", good, "Bad pieces:", bad)

	left := int64(bad) * int64(t.m.Info.PieceLength)
	if !t.pieceSet.IsSet(t.totalPieces - 1) {
		left = left - t.m.Info.PieceLength + int64(t.lastPieceLength)
	}
	t.si = &SessionInfo{PeerId: peerId(), Port: listenPort, Left: left}
	if useDHT {
		// TODO: UPnP UDP port mapping.
		if t.dht, err = dht.NewDHTNode(listenPort, cfg.TARGET_NUM_PEERS, true); err != nil {
			log.Println("DHT node creation error", err)
			return
		}
		go t.dht.DoDHT()
	}

	go IoRoutine(t.ioRequestChan, t.ioResponceChan)

	return t, err
}

func (t *TorrentSession) isSeeding() bool{
	return t.goodPieces == t.totalPieces
}

func (t *TorrentSession) fetchTrackerInfo(event string) {
	m, si := t.m, t.si
	//log.Println("Stats: Uploaded", si.Uploaded, "Downloaded", si.Downloaded, "Left", si.Left)
	u, err := url.Parse(m.Announce)
	if err != nil {
		log.Println("Error: Invalid announce URL(", m.Announce, "):", err)
	}
	uq := u.Query()
	uq.Add("info_hash", m.InfoHash)
	uq.Add("peer_id", si.PeerId)
	uq.Add("port", strconv.Itoa(si.Port))
	uq.Add("uploaded", strconv.FormatInt(si.Uploaded, 10))
	uq.Add("downloaded", strconv.FormatInt(si.Downloaded, 10))
	uq.Add("left", strconv.FormatInt(si.Left, 10))
	uq.Add("compact", "1")

	if event != "" {
		uq.Add("event", event)
	}

	// This might reorder the existing query string in the Announce url
	// I worry this might break some broken trackers that don't parse URLs
	// properly.

	u.RawQuery = uq.Encode()

	ch := t.trackerInfoChan
	go func() {
		ti, err := getTrackerInfo(u.String())
		if ti == nil || err != nil {
			log.Println("Error: Could not fetch tracker info:", err)
		} else if ti.FailureReason != "" {
			log.Println("Error: Tracker returned failure reason:", ti.FailureReason)
		} else {
			ch <- ti
		}
	}()
}

func connectToPeer(peer string, ch chan net.Conn) {
	// log.Println("Connecting to", peer)
	conn, err := proxyNetDial("tcp", peer)
	if err != nil {
		log.Println("Failed to connect to", peer, err)
	} else {
		log.Println("Connected to", peer)
		ch <- conn
	}
}

func (t *TorrentSession) PeerExist(address string) (exist bool) {
	index := strings.Index(address, ":")
	ip := address[:index]
	_, exist = t.peers[ip]
	return exist
}

func (t *TorrentSession) PeerCount() (count int) {
	return len(t.peers)
}

func (t *TorrentSession) RemovePeer(address string) {
	index := strings.Index(address, ":")
	ip := address[:index]
	delete(t.peers, ip)
}

func (t *TorrentSession) AddPeer(conn net.Conn) {
	address := conn.RemoteAddr().String()
	
	index := strings.Index(address, ":")
	ip := address[:index]

	if t.PeerExist(address) {
		log.Println("already exist a connection", address)
		conn.Close()
		return
	}

	if t.PeerCount() >= cfg.MAX_NUM_PEERS {
		log.Println("We have enough peers. Rejecting additional peer", address)
		conn.Close()
		return
	}

	log.Println("Adding peer", address)

	ps := NewPeerState(conn)
	ps.address = address
	var header [68]byte
	copy(header[0:], kBitTorrentHeader[0:])
	if t.m.Info.Private != 1 && useDHT {
		header[27] = header[27] | 0x01
	}
	copy(header[28:48], string2Bytes(t.m.InfoHash))
	copy(header[48:68], string2Bytes(t.si.PeerId))

	t.peers[ip] = ps
	go ps.peerWriter(t.peerMessageChan, header[0:])
	go ps.peerReader(t.peerMessageChan)
}

func (t *TorrentSession) ClosePeer(peer *peerState) {
	log.Println("Closing peer", peer.address)
	_ = t.removeRequests(peer)
	peer.Close()
	t.RemovePeer(peer.address)
}



//todo: refactor
func (t *TorrentSession) SchedulerChokeUnchoke() {
	//(1). When leeching
	// x是同时允许传输数据的peer数量
	//1. 按照给我上传的速度排序，若有m个peer，每次choke掉N(N < x)个速度慢的peer，并从之前处于choke状态的peer中随机选择N个peer执行unchoke
	//2. 反对冷落，后起动的peer可能完全没有数据，按照算法1的处理方式，后启动的peer可能比较冷，考虑每隔一段时间对速度最慢的peer执行unchoke

	//(2). when seeding
	// Unchoke the top 'MaxUploads' downloaders (peers that we are
	// uploading to) and choke all others.

	vec := make([]*peerState, 0)

	if t.isSeeding() {
		for _, v := range t.peers{
			//insertion sort
			if !v.peer_interested || v.isSeed {
				continue
			}
			
			i := 0
			for ; i < len(vec); i++ {
				 if v.upload >= vec[i].upload {	//find position
					break
				}
			}

			vec = append(vec[:i], append([]*peerState{v}, vec[i:]...)...)
			//log.Println(v.address, "download ", v.download, "upload", v.upload)
		}

	}else{
		for _, v := range t.peers{
			//insertion sort
			if !v.peer_interested || v.isSeed {
				continue
			}

			i := 0
			for ; i < len(vec); i++ {
				 if v.download >= vec[i].download {	//find position
					break
				}
			}

			vec = append(vec[:i], append([]*peerState{v}, vec[i:]...)...)
			//log.Println(v.address, "download ", v.download, "upload", v.upload)
		}
	}

	interestedCount := len(vec)
	log.Println("interestedCount", interestedCount, "peer count", len(t.peers))

	for _, v := range t.peers{
		if !v.peer_interested && !v.isSeed {
			vec = append(vec, v)
		}
	}

	if t.isSeeding() {
		n := cfg.MAX_UPLOADING_CONNECTION
		//todo: refactor to function
		if interestedCount <= n {
			for i := 0; i < interestedCount; i++ {	//unchoke all
				log.Println("unchoke ", vec[i].address, "download", vec[i].download, "upload", vec[i].upload, "am_choking", vec[i].am_choking)
				vec[i].SetChoke(false)
			}

			//choke all others
			for i := interestedCount; i < len(vec); i++ {	
				log.Println("choke ", vec[i].address, "download", vec[i].download, "upload", vec[i].upload, "am_choking", vec[i].am_choking)
				vec[i].SetChoke(true)
			}
		}else{
			for i := 0; i < n; i++ {
				log.Println("unchoke ", vec[i].address, "download", vec[i].download, "upload", vec[i].upload, "am_choking", vec[i].am_choking)
				vec[i].SetChoke(false)
			}

			left := vec[n : interestedCount]
			//随机unchoke一个连接
			opti := rand.Intn(len(left));
			
			log.Println("optimistic unchoke ", left[opti].address, "download", left[opti].download, "upload", left[opti].upload, "am_choking", vec[opti].am_choking)
			left[opti].SetChoke(false)

			//choke all others
			for i := n; i < len(vec); i++ {	
				if i != n + opti {
					log.Println("choke ", vec[i].address, "download", vec[i].download, "upload", vec[i].upload, "am_choking", vec[i].am_choking)
					vec[i].SetChoke(true)
				}
			}
		}
	}else{
		n := cfg.MAX_DOWNLOADING_CONNECTION
		//todo: refactor to function
		if interestedCount <= n {
			for i := 0; i < interestedCount; i++ {	//unchoke all
				log.Println("unchoke ", vec[i].address, "download", vec[i].download, "upload", vec[i].upload, "am_choking", vec[i].am_choking)
				vec[i].SetChoke(false)
			}

			//choke all others
			for i := interestedCount; i < len(vec); i++ {	
				log.Println("choke ", vec[i].address, "download", vec[i].download, "upload", vec[i].upload, "am_choking", vec[i].am_choking)
				vec[i].SetChoke(true)
			}
		}else{
			for i := 0; i < n; i++ {
				log.Println("unchoke ", vec[i].address, "download", vec[i].download, "upload", vec[i].upload, "am_choking", vec[i].am_choking)
				vec[i].SetChoke(false)
			}

			left := vec[n : interestedCount]
			//随机unchoke一个连接
			opti := rand.Intn(len(left));
			
			log.Println("optimistic unchoke ", left[opti].address, "download", left[opti].download, "upload", left[opti].upload, "am_choking", vec[opti].am_choking)
			left[opti].SetChoke(false)

			//choke all others
			for i := n; i < len(vec); i++ {	
				if i != n + opti {
					log.Println("choke ", vec[i].address, "download", vec[i].download, "upload", vec[i].upload, "am_choking", vec[i].am_choking)
					vec[i].SetChoke(true)
				}
			}
		}
	}
	
	
	//reset
	for _, ps := range t.peers{
			ps.upload = 0;
			ps.download = 0;
			ps.lastSchedule = time.Now()
	}
}


func (t *TorrentSession) DoTorrent() (err error) {
	t.lastHeartBeat = time.Now()

	log.Println("Fetching torrent.")
	rechokeChan := time.Tick(10 * time.Second)
	// Start out polling tracker every 20 seconds untill we get a response.
	// Maybe be exponential backoff here?
	retrackerChan := time.Tick(20 * time.Second)
	keepAliveChan := time.Tick(60 * time.Second)
	t.trackerInfoChan = make(chan *TrackerResponse)

	conChan := make(chan net.Conn)
	if !useProxy() {
		// Only listen for peer connections if not using a proxy.
		t.listenForPeerConnections(conChan)
	}

	DHTPeersRequestResults := make(chan map[string][]string)
	if t.m.Info.Private != 1 && useDHT {
		DHTPeersRequestResults = t.dht.PeersRequestResults
		t.dht.PeersRequest(t.m.InfoHash, true)
	}

	if !t.isSeeding() {
		t.fetchTrackerInfo("started")
	}

	for {
		select {
		case _ = <-retrackerChan:
			if !trackerLessMode {
				t.fetchTrackerInfo("")
			}
		case dhtInfoHashPeers := <-DHTPeersRequestResults:
			newPeerCount := 0
			// key = infoHash. The torrent client currently only
			// supports one download at a time, so let's assume
			// it's the case.
			for _, peers := range dhtInfoHashPeers {
				for _, peer := range peers {
					peer = nettools.BinaryToDottedPort(peer)
					if !t.PeerExist(peer) {
						newPeerCount++
						go connectToPeer(peer, conChan)
					}
				}
			}
			// log.Println("Contacting", newPeerCount, "new peers (thanks DHT!)")
		case ti := <-t.trackerInfoChan:
			t.ti = ti
			//log.Println("Torrent has", t.ti.Complete, "seeders and", t.ti.Incomplete, "leachers.")
			if !trackerLessMode {
				peers := t.ti.Peers
				//log.Println("Tracker gave us", len(peers)/6, "peers")
				newPeerCount := 0
				for i := 0; i < len(peers); i += 6 {
					peer := nettools.BinaryToDottedPort(peers[i : i+6])
					//log.Printf("trackerInfoChan %v\n", peer)
					selfIp, err := GetLocalIP(); 
					if err != nil{
						log.Println(err)
						panic("")
					}
					
					if !t.PeerExist(peer) && !strings.Contains(peer, selfIp) {
						newPeerCount++
						go connectToPeer(peer, conChan)
					}
				}
				//log.Println("Contacting", newPeerCount, "new peers")
				interval := t.ti.Interval
				if interval < 120 {
					interval = 10;
				} else if interval > 24*3600 {
					interval = 24 * 3600
				}
				//log.Println("..checking again in", interval, "seconds.")
				retrackerChan = time.Tick(interval * time.Second)
				//log.Println("Contacting", newPeerCount, "new peers")
			}
			interval := t.ti.Interval
			if interval < 120 {
				interval = 10
			} else if interval > 24*3600 {
				interval = 24 * 3600
			}
			//log.Println("..checking again in", interval.String())
			retrackerChan = time.Tick(interval * time.Second)

		case pm := <-t.peerMessageChan:
			peer, message := pm.peer, pm.message
			peer.lastReadTime = time.Now()
			err2 := t.DoMessage(peer, message)
			if err2 != nil {
				if err2 != io.EOF {
					log.Println("Closing peer", peer.address, "because", err2)
				}
				t.ClosePeer(peer)
			}
		case conn := <-conChan:
			t.AddPeer(conn)
		case ioResult := <-t.ioResponceChan:
			if _, ok := ioResult.(*WriteContext); ok {
				
			}else if context, ok := ioResult.(*ReadContext); ok {
				value := &CacheValue{}
				value.buf = append(value.buf, context.msgBuf[9:]...)

				t.cache.Set(context.globalOffset, value)
				//make sure the peer is still alive
				for _, v := range t.peers{
					if v == context.peer{
						context.peer.sendMessage(context.msgBuf)
						t.si.Uploaded += int64(context.length)
					}
				}
			}else{
				panic("Unknown message")
			}	
		case _ = <-rechokeChan:
			/*
			t.lastHeartBeat = time.Now()
			ratio := float64(0.0)
			if t.si.Downloaded > 0 {
				ratio = float64(t.si.Uploaded) / float64(t.si.Downloaded)
			}
			log.Println("Peers:", t.PeerCount(), "downloaded:", t.si.Downloaded, "uploaded:", t.si.Uploaded, "ratio", ratio)
			log.Println("good, total", t.goodPieces, t.totalPieces)
			*/
			if t.PeerCount() < cfg.TARGET_NUM_PEERS && t.goodPieces < t.totalPieces {
				if t.m.Info.Private != 1 && useDHT {
					go t.dht.PeersRequest(t.m.InfoHash, true)
				}
				if !trackerLessMode {
					if t.ti == nil || t.ti.Complete > 100 {
						t.fetchTrackerInfo("")
					}
				}
			}

			t.SchedulerChokeUnchoke()

		case _ = <-keepAliveChan:
			now := time.Now()
			for _, peer := range t.peers {
				if peer.lastReadTime.Second() != 0 && now.Sub(peer.lastReadTime) > 3*time.Minute {
					log.Println("Closing peer", peer.address, "because timed out.")
					t.ClosePeer(peer)
					continue
				}
				err2 := t.doCheckRequests(peer)
				if err2 != nil {
					if err2 != io.EOF {
						log.Println("Closing peer", peer.address, "because", err2)
					}
					t.ClosePeer(peer)
					continue
				}
				peer.keepAlive(now)
			}
		}
	}
	return
}

func (t *TorrentSession) RequestBlock(p *peerState) (err error) {
	for k, _ := range t.activePieces {
		if p.have.IsSet(k) {
			err = t.RequestBlock2(p, k, false)
			if err != io.EOF {
				return
			}
		}
	}
	// No active pieces. (Or no suitable active pieces.) Pick one
	piece := t.ChoosePiece(p)
	/*
	if piece < 0 {
		// No unclaimed pieces. See if we can double-up on an active piece
		for k, _ := range t.activePieces {
			if p.have.IsSet(k) {
				err = t.RequestBlock2(p, k, true)
				if err != io.EOF {
					return
				}
			}
		}
	}
	*/

	if piece >= 0 {
		pieceLength := int(t.m.Info.PieceLength)
		if piece == t.totalPieces-1 {
			pieceLength = t.lastPieceLength
		}
		pieceCount := (pieceLength + STANDARD_BLOCK_LENGTH - 1) / STANDARD_BLOCK_LENGTH
		t.activePieces[piece] = &ActivePiece{make([]int, pieceCount), pieceLength}
		return t.RequestBlock2(p, piece, false)
	} else {
		p.SetInterested(false)
	}
	return nil
}

func (t *TorrentSession) ChoosePiece(p *peerState) (piece int) {
	n := t.totalPieces
	start := rand.Intn(n)
	piece = t.checkRange(p, start, n)
	if piece == -1 {
		piece = t.checkRange(p, 0, start)
	}
	
	return
}

func (t *TorrentSession) checkRange(p *peerState, start, end int) (piece int) {
	for i := start; i < end; i++ {
		if (!t.pieceSet.IsSet(i)) && p.have.IsSet(i) {
			if _, ok := t.activePieces[i]; !ok {
				return i
			}
		}
	}
	return -1
}

func (t *TorrentSession) RequestBlock2(p *peerState, piece int, endGame bool) (err error) {
	v := t.activePieces[piece]
	block := v.chooseBlockToDownload(endGame)
	if block >= 0 {
		//log.Println("request piece", piece, "block", block, "from", p.address)
		t.requestBlockImp(p, piece, block, true)
	} else {
		return io.EOF
	}
	return
}


// Request or cancel a block
func (t *TorrentSession) requestBlockImp(p *peerState, piece int, block int, request bool) {
	begin := block * STANDARD_BLOCK_LENGTH
	req := make([]byte, 13)
	opcode := byte(6)
	if !request {
		opcode = byte(8) // Cancel
	}
	length := STANDARD_BLOCK_LENGTH
	if piece == t.totalPieces-1 {
		left := t.lastPieceLength - begin
		if left < length {
			length = left
		}
	}
	// log.Println("Requesting block", piece, ".", block, length, request)
	req[0] = opcode
	uint32ToBytes(req[1:5], uint32(piece))
	uint32ToBytes(req[5:9], uint32(begin))
	uint32ToBytes(req[9:13], uint32(length))
	requestIndex := (uint64(piece) << 32) | uint64(begin)
	if !request {
		delete(p.our_requests, requestIndex)
	} else {
		p.our_requests[requestIndex] = time.Now()
	}
	p.sendMessage(req)
	return
}

func (t *TorrentSession) RecordBlock(p *peerState, piece, begin, length uint32) (err error) {
	block := begin / STANDARD_BLOCK_LENGTH
	// log.Println("Received block", piece, ".", block)
	requestIndex := (uint64(piece) << 32) | uint64(begin)
	delete(p.our_requests, requestIndex)
	v, ok := t.activePieces[int(piece)]
	if ok {
		requestCount := v.recordBlock(int(block))
		if requestCount > 1 {
			// Someone else has also requested this, so send cancel notices
			for _, peer := range t.peers {
				if p != peer {
					if _, ok := peer.our_requests[requestIndex]; ok {
						t.requestBlockImp(peer, int(piece), int(block), false)
						requestCount--
					}
				}
			}
		}
		t.si.Downloaded += int64(length)
		if v.isComplete() {
			delete(t.activePieces, int(piece))
			ok, err = checkPiece(t.fileStore, t.totalSize, t.m, int(piece))
			if !ok || err != nil {
				log.Println("Ignoring bad piece", piece, err)
				return
			}
			t.si.Left -= int64(v.pieceLength)
			t.pieceSet.Set(int(piece))
			t.goodPieces++
			//log.Println("Have", t.goodPieces, "of", t.totalPieces, "pieces.")
			if t.isSeeding() {
				log.Printf("\n\n\ndownload finish\n\n\n")
				t.fetchTrackerInfo("completed")
				// TODO: Drop connections to all seeders.
				for _, p := range t.peers {
					p.SetInterested(false)
				}
			}

			for _, p := range t.peers {
				if p.have != nil {
					if p.have.IsSet(int(piece)) {
						// We don't do anything special. We rely on the caller
						// to decide if this peer is still interesting.
					} else {
						//log.Println("...telling ", p)
						haveMsg := make([]byte, 5)
						haveMsg[0] = 4
						uint32ToBytes(haveMsg[1:5], uint32(piece))
						p.sendMessage(haveMsg)
					}
				}
			}
		}
	} else {
		log.Println("Received a block we already have.", piece, block, p.address)
	}
	return
}

func (t *TorrentSession) doChoke(p *peerState) (err error) {
	p.peer_choking = true
	err = t.removeRequests(p)
	return
}

func (t *TorrentSession) removeRequests(p *peerState) (err error) {
	for k, _ := range p.our_requests {
		piece := int(k >> 32)
		begin := int(k)
		block := begin / STANDARD_BLOCK_LENGTH
		// log.Println("Forgetting we requested block ", piece, ".", block)
		t.removeRequest(piece, block)
	}
	p.our_requests = make(map[uint64]time.Time, cfg.MAX_OUR_REQUESTS)
	return
}

func (t *TorrentSession) removeRequest(piece, block int) {
	v, ok := t.activePieces[piece]
	if ok && v.downloaderCount[block] > 0 {
		v.downloaderCount[block]--
	}
}

func (t *TorrentSession) doCheckRequests(p *peerState) (err error) {
	now := time.Now()
	for k, v := range p.our_requests {
		if now.Sub(v).Seconds() > 30 {
			piece := int(k >> 32)
			block := int(k) / STANDARD_BLOCK_LENGTH
			log.Println("timing out request of", piece, ".", block, "from", p.address)
			t.removeRequest(piece, block)
		}
	}
	return
}



func (t *TorrentSession) DoMessage(p *peerState, message []byte) (err error) {
	if message == nil {
		return io.EOF // The reader or writer goroutine has exited
	}
	if len(p.id) == 0 {
		// This is the header message from the peer.
		if t.m.Info.Private != 1 && useDHT {
			// If 128, then it supports DHT.
			if int(message[7])&0x01 == 0x01 {
				// It's OK if we know this node already. The DHT engine will
				// ignore it accordingly.
				go t.dht.RemoteNodeAcquaintance(p.address)
			}
		}

		peersInfoHash := string(message[8:28])
		if peersInfoHash != t.m.InfoHash {
			return errors.New("this peer doesn't have the right info hash")
		}
		p.id = string(message[28:48])

		//LiuQi: exclude self
		if p.id == t.si.PeerId {
			err = errors.New("exclude self")
			return
		}

		//LiuQi: exchange bitfield
		length := 1 + (t.totalPieces + 7) / 8
		bitfieldMsg := make([]byte, length) 
		uint32ToBytes(bitfieldMsg, uint32(length))
		bitfieldMsg[0] = BITFIELD
		copy(bitfieldMsg[1:], t.pieceSet.b)
		log.Println("send bitfield to", p.address)
		p.sendMessage(bitfieldMsg)

	} else {
		if len(message) == 0 { // keep alive
			return
		}
		messageId := message[0]
		// Message 5 is optional, but must be sent as the first message.
		if p.have == nil && messageId != 5 {
			// Fill out the have bitfield
			p.have = NewBitset(t.totalPieces)
		}
		switch id := message[0]; id {
		case CHOKE:
			log.Println("choke from", p.address)
			if len(message) != 1 {
				return errors.New("Unexpected length")
			}
			err = t.doChoke(p)
		case UNCHOKE:
			log.Println("unchoke from", p.address)
			if len(message) != 1 {
				return errors.New("Unexpected length")
			}
			p.peer_choking = false
			for i := 0; i < cfg.MAX_OUR_REQUESTS; i++ {
				err = t.RequestBlock(p)
				if err != nil {
					return
				}
			}
		case INTERESTED:
			log.Println("interested from", p.address)
			if len(message) != 1 {
				return errors.New("Unexpected length")
			}
			p.peer_interested = true
			//p.SetChoke(true)
		case NOT_INTERESTED:
			log.Println("not interested from", p.address)
			if len(message) != 1 {
				return errors.New("Unexpected length")
			}
			p.peer_interested = false
			p.SetChoke(true)
		case HAVE:
			if len(message) != 5 {
				return errors.New("Unexpected length")
			}

			n := bytesToUint32(message[1:])

//			log.Println("have", n, "from", p.address)
			if n < uint32(p.have.n) {
				p.have.Set(int(n))
				if !p.am_interested && !t.pieceSet.IsSet(int(n)) {
					p.SetInterested(true)
				}
			} else {
				return errors.New("have index is out of range.")
			}
		case BITFIELD:
			log.Println("bitfield from", p.address)
			if p.have != nil {
				return errors.New("Late bitfield operation")
			}
			p.have = NewBitsetFromBytes(t.totalPieces, message[1:])
			if p.have == nil {
				return errors.New("Invalid bitfield data.")
			}

			//is this a seeding peer 
			i := 0
			for ; i < t.totalPieces; i++ {
				if !p.have.IsSet(i) {
					break
				}
			}
			if i == t.totalPieces{
				p.isSeed = true
				log.Println("we got a seed", p.address)
			}else{
				p.SetChoke(true) // TODO: better choke policy
			}

			t.checkInteresting(p)
		case REQUEST:
			// log.Println("request", p.address)
			if len(message) != 13 {
				return errors.New("Unexpected message length")
			}
			index := bytesToUint32(message[1:5])
			begin := bytesToUint32(message[5:9])
			length := bytesToUint32(message[9:13])
			if index >= uint32(p.have.n) {
				return errors.New("piece out of range.")
			}
			if !t.pieceSet.IsSet(int(index)) {
				return errors.New("we don't have that piece.")
			}
			if int64(begin) >= t.m.Info.PieceLength {
				return errors.New("begin out of range.")
			}
			if int64(begin)+int64(length) > t.m.Info.PieceLength {
				return errors.New("begin + length out of range.")
			}
			if length != STANDARD_BLOCK_LENGTH {
				return errors.New("Unexpected block length.")
			}
			p.upload += int(length) / 1024	//k
			if _, ok := t.history[p.address]; !ok {
				t.history[p.address] = &DownloadUpload{0, 0}
			}

			t.history[p.address].Uploaded += int64(length)
			t.sendRequest(p, index, begin, length)
		case PIECE:
			// piece
			if len(message) < 9 {
				return errors.New("unexpected message length")
			}
			index := bytesToUint32(message[1:5])
			begin := bytesToUint32(message[5:9])
			length := len(message) - 9
			if index >= uint32(p.have.n) {
				return errors.New("piece out of range.")
			}
			if t.pieceSet.IsSet(int(index)) {	//timeout will cause this
				log.Println("We already have that piece", p.address)
				// We already have that piece, keep going
				//err = t.RequestBlock(p)
				break
			}
			if int64(begin) >= t.m.Info.PieceLength {
				return errors.New("begin out of range.")
			}
			if int64(begin)+int64(length) > t.m.Info.PieceLength {
				return errors.New("begin + length out of range.")
			}
			if length > 128*1024 {
				return errors.New("Block length too large.")
			}
			globalOffset := int64(index)*t.m.Info.PieceLength + int64(begin)

			p.download += int(length) / 1024	// k
			if _, ok := t.history[p.address]; !ok {
				t.history[p.address] = &DownloadUpload{0, 0}
			}
			t.history[p.address].Downloaded += int64(length)

			context := &WriteContext{peer:p, whichPiece:index, begin:begin, length:uint32(length)}
			args := &IoArgs{ f : t.fileStore, ioMode:MODE_WRITE, buf: message[9:], offset:globalOffset, context:context}  
			//todo: check if we still interested in other peers

			value := &CacheValue{}
			value.buf = append(value.buf, message[9:]...)
			
			t.cache.Set(globalOffset, value)


			//asyn write
			t.ioRequestChan<-args

			t.RecordBlock(p, index, begin, uint32(length))
			err = t.RequestBlock(p)
			
		case CANCEL:
			// log.Println("cancel")
			if len(message) != 13 {
				return errors.New("Unexpected message length")
			}
			index := bytesToUint32(message[1:5])
			begin := bytesToUint32(message[5:9])
			length := bytesToUint32(message[9:13])
			if index >= uint32(p.have.n) {
				return errors.New("piece out of range.")
			}
			if !t.pieceSet.IsSet(int(index)) {
				return errors.New("we don't have that piece.")
			}
			if int64(begin) >= t.m.Info.PieceLength {
				return errors.New("begin out of range.")
			}
			if int64(begin)+int64(length) > t.m.Info.PieceLength {
				return errors.New("begin + length out of range.")
			}
			if length != STANDARD_BLOCK_LENGTH {
				return errors.New("Unexpected block length.")
			}
			//just ignore it

		case PORT:
			// TODO: Implement this message.
			// We see peers sending us 16K byte messages here, so
			// it seems that we don't understand what this is.
			if len(message) != 3 {
				return errors.New(fmt.Sprintf("Unexpected length for port message:", len(message)))
			}
			go t.dht.RemoteNodeAcquaintance(p.address)
		default:
			return errors.New("Uknown message id")
		}
	}
	return
}


func (t *TorrentSession) sendRequest(peer *peerState, index, begin, length uint32) (err error) {
	if !peer.am_choking {
		// log.Println("Sending block", index, begin)
		buf := make([]byte, length+9)
		buf[0] = 7
		uint32ToBytes(buf[1:5], index)
		uint32ToBytes(buf[5:9], begin)
		
		globalOffset := int64(index)*t.m.Info.PieceLength+int64(begin)
		if block, ok := t.cache.Get(globalOffset); ok{
			//log.Println("cache hint")
			if k, ok := block.(*CacheValue); ok {
				copy(buf[9:], k.buf)
				peer.sendMessage(buf)
				t.si.Uploaded += int64(length)
			}else{
				panic("invalid cache")
			}
		}else{
			//log.Println("cache miss ", globalOffset)
			//asyn read
			rc := &ReadContext{peer:peer, msgBuf:buf, globalOffset:globalOffset, length:length}
			args := &IoArgs{ f : t.fileStore, ioMode:MODE_READ, buf: buf[9:], offset:globalOffset, context:rc}  
			t.ioRequestChan<-args
		}
	}
	return
}

func (t *TorrentSession) checkInteresting(p *peerState) {
	p.SetInterested(t.isInteresting(p))
}

func (t *TorrentSession) isInteresting(p *peerState) bool {
	for i := 0; i < t.totalPieces; i++ {
		if !t.pieceSet.IsSet(i) && p.have.IsSet(i) {
			return true
		}
	}
	return false
}
