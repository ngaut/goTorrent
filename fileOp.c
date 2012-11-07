#include "fileOp.h"
#include <stdio.h>
#include <fcntl.h>


#define _FILE_OFFSET_BITS 64

void ShowMessage(char* str)
{
	printf("%s\n", str);
	MessageBoxA(NULL, str, "", MB_OK);
}



int ReadAt(int fd, void* buf, int len, __int64 offset){
	int n = _lseeki64(fd, offset, SEEK_SET);
	if (n == -1)
	{
		printf("_lseeki64 errno %d\n", errno);
		ShowMessage("oops _lseeki64 failed");
	}
	n = _read(fd, buf, len);

	//printf("ReadAt %I64d\n", offset);

	if (n <= 0)
	{
		printf("errno %d\n", errno);
		ShowMessage("oops");
	}

	return n;
}

int WriteAt(int fd, void* buf, int len, __int64 offset){
	int n = _lseeki64(fd, offset, SEEK_SET);
	if (n == -1)
	{
		printf("_lseeki64 errno %d\n", errno);
		ShowMessage("oops");
	}
	n = _write(fd, buf, len);

	//printf("WriteAt %I64d\n", offset);

	if (n <= 0)
	{
		printf("errno %d\n", errno);
		ShowMessage("oops");
	}

	return n;
}

int Open(const char* fname, int oflag){
	return _open(fname, oflag);
}

void Close(int fd){
	ShowMessage("close file");
	_close(fd);
}
