package main

import (
	"log"
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

		responce<-arg.context
	}
	log.Println("exit IoRoutine")
}