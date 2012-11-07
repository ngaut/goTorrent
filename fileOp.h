
#ifndef FILE_OP_H

#include <windows.h>

void ShowMessage(char* str);
int ReadAt(int fd, void* buf, int len, __int64 offset);
int WriteAt(int fd, void* buf, int len, __int64 offset);
int Open(const char* fname, int oflag);
void Close(int fd);

#endif