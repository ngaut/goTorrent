#include "fileOp.h"
#include <stdio.h>
#include <fcntl.h>


#define _FILE_OFFSET_BITS 64

void ShowMessage(char* str)
{
  printf("%s\n", str);
  MessageBoxA(NULL, str, "", MB_OK);
}



int ReadAt(void* h, void* buf, int len, __int64 offset){
	FILE* f = (FILE*)h;
	_lseeki64(f, offset, SEEK_SET);
	int n = fread(buf, 1, len, f);
	//printf("ReadAt %I64d\n", offset);

	return n;
}

int WriteAt(void* h, void* buf, int len, __int64 offset){
	FILE* f = (FILE*)h;
	_lseeki64(f, offset, SEEK_SET);
	int n = fwrite(buf, 1, len, f);
	//printf("WriteAt %I64d\n", offset);

	return n;
}

void* Open(const char* fname, const char* mode){
	return (void*)fopen(fname, mode);
}

void Close(void* h){
	fclose((FILE*)h);
}
