package main

import (
	"log"
	"time"
)

const(
	MODE_READ 	= 1
	MODE_WRITE	= 2
)

type IoArgs struct{
	f 		FileStore
	ioMode	int
	buf 	[]byte
	offset 	int64
	context	interface{}
}

type WriteContext struct{
	peer 		*peerState
	whichPiece 	uint32	//piece index
	begin 		uint32
	length 		uint32
}

type ReadContext struct{
	peer 			*peerState
	msgBuf			[]byte
	globalOffset 	int64
	length 			uint32
}


func IoRoutine(request <-chan *IoArgs, responce chan<- interface{} ) {
	log.Println("start IoRoutine")
	for arg := range request {
		start := time.Now()
		if cfg.doRealReadWrite {
			if arg.ioMode == MODE_READ{
				_, err := arg.f.ReadAt(arg.buf, arg.offset)
				if err != nil {
					panic("")
				}
			}else{	//write
				_, err := arg.f.WriteAt(arg.buf, arg.offset)
				if err != nil {
					panic("")
				}
			}
		}
		
		sec := time.Now().Sub(start).Seconds()
		if sec >= 2 {
			var mod = "READ"
			if arg.ioMode == MODE_WRITE {
				mod = "WRITE"
			}
			log.Println("\nwarning, disk io too slow, use %v seconds, mod:%v, offset:%v\n", sec,  mod, arg.offset)
		}
		responce<-arg.context
	}
	log.Println("exit IoRoutine")
}