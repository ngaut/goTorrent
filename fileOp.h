
#ifndef FILE_OP_H

#include <windows.h>

void ShowMessage(char* str);
int ReadAt(void* h, void* buf, int len, __int64 offset);
int WriteAt(void* h, void* buf, int len, __int64 offset);
void* Open(const char* fname, const char* mode);
void Close(void* h);

#endif