package main

/*
#include "fileOp.h"
*/
import "C"
import (
	"errors"
	"io"
	"os"
	"path"
	"log"
	"strings"
	"unsafe"
)

const O_BINARY = 0x8000 

type FileStore interface {
	io.ReaderAt
	io.WriterAt
	io.Closer
}

type fileEntry struct {
	length int64
	fd     int
}

type fileStore struct {
	offsets []int64
	files   []fileEntry // Stored in increasing globalOffset order
}


func (fe *fileEntry) open(name string, length int64) (err error) {
	fe.length = length
	_, e := os.Stat(name)
	if e != nil && os.IsNotExist(e) {
		_, err = os.Create(name)
 		if err != nil {
 			return
		}
	} 

	 err = os.Truncate(name, length)
 	if err != nil {
 		return
 	}

	fe.fd = int(C.Open(C.CString(name), _Ctype_int(os.O_RDWR | O_BINARY)))
	log.Println("fd", fe.fd)
	return nil
}

func ensureDirectory(fullPath string) (err error) {
	fullPath = path.Clean(fullPath)
	if !strings.HasPrefix(fullPath, "/") {
		// Transform into absolute path.
		var cwd string
		if cwd, err = os.Getwd(); err != nil {
			return
		}
		fullPath = cwd + "/" + fullPath
	}
	base, _ := path.Split(fullPath)
	if base == "" {
		panic("Programming error: could not find base directory for absolute path " + fullPath)
	}
	err = os.MkdirAll(base, 0755)
	return
}

func NewFileStore(info *InfoDict, storePath string) (f FileStore, totalSize int64, err error) {
	fs := new(fileStore)
	numFiles := len(info.Files)
	if numFiles == 0 {
		// Create dummy Files structure.
		info = &InfoDict{Files: []FileDict{FileDict{info.Length, []string{info.Name}, info.Md5sum}}}
		numFiles = 1
	}
	fs.files = make([]fileEntry, numFiles)
	fs.offsets = make([]int64, numFiles)
	for i, _ := range info.Files {
		src := &info.Files[i]
		fullPath := path.Join(storePath, path.Clean(path.Join(src.Path...)))
		err = ensureDirectory(fullPath)
		if err != nil {
			return
		}
		err = fs.files[i].open(fullPath, src.Length)
		if err != nil {
			return
		}
		fs.offsets[i] = totalSize
		totalSize += src.Length
	}
	f = fs
	return
}

func (f *fileStore) find(offset int64) int {
	// Binary search
	offsets := f.offsets
	low := 0
	high := len(offsets)
	for low < high-1 {
		probe := (low + high) / 2
		entry := offsets[probe]
		if offset < entry {
			high = probe
		} else {
			low = probe
		}
	}
	return low
}

func (f *fileStore) ReadAt(p []byte, off int64) (n int, err error) {
	index := f.find(off)
	for len(p) > 0 && index < len(f.offsets) {
		chunk := int64(len(p))
		entry := &f.files[index]
		itemOffset := off - f.offsets[index]
		if itemOffset < entry.length {
			space := entry.length - itemOffset
			if space < chunk {
				chunk = space
			}
			fd := entry.fd
			var nThisTime int
			/*nThisTime, err = fd.ReadAt(p[0:chunk], itemOffset)
			n = n + nThisTime
			if err != nil {
				return
			}
			*/

			nThisTime = int(C.ReadAt(_Ctype_int(fd), unsafe.Pointer(&p[0]), _Ctype_int(chunk), _Ctype_longlong(itemOffset)))
			if nThisTime <= 0 {
				//log.Println("read file failed, offset", itemOffset, "fd", fd)
				//panic("")
				return
			}
			p = p[nThisTime:]
			off += int64(nThisTime)
		}
		index++
	}
	// At this point if there's anything left to read it means we've run off the
	// end of the file store. Read zeros. This is defined by the bittorrent protocol.
	for i, _ := range p {
		p[i] = 0
	}
	return
}

func (f *fileStore) WriteAt(p []byte, off int64) (n int, err error) {
	index := f.find(off)
	for len(p) > 0 && index < len(f.offsets) {
		chunk := int64(len(p))
		entry := &f.files[index]
		itemOffset := off - f.offsets[index]
		if itemOffset < entry.length {
			space := entry.length - itemOffset
			if space < chunk {
				chunk = space
			}
			fd := entry.fd
			var nThisTime int
			/*
			nThisTime, err = fd.WriteAt(p[0:chunk], itemOffset)
			n += nThisTime
			if err != nil {
				return
			}
			*/
			nThisTime = int(C.WriteAt(_Ctype_int(fd), unsafe.Pointer(&p[0]), _Ctype_int(chunk), _Ctype_longlong(itemOffset)))
			if nThisTime < 0 {
				//log.Println("Write file failed")
				panic("")
				return
			}

			p = p[nThisTime:]
			off += int64(nThisTime)
		}
		index++
	}
	// At this point if there's anything left to write it means we've run off the
	// end of the file store. Check that the data is zeros.
	// This is defined by the bittorrent protocol.
	for i, _ := range p {
		if p[i] != 0 {
			err = errors.New("Unexpected non-zero data at end of store.")
			n = n + i
			return
		}
	}
	n = n + len(p)
	return
}

func (f *fileStore) Close() (err error) {
	for i, _ := range f.files {
		fd := f.files[i].fd
		if fd != 0 {
			//fd.Close()
			C.Close(_Ctype_int(fd))
			f.files[i].fd = 0
		}
	}
	return
}
