// Included ahead of every vendored source (-include, microfeatures.go): the
// frontend builds three tables at set-up with log1pf (the filterbank), cosf
// (the window) and powf (PCAN's gain), and their last bit is the C
// library's. glibc 2.39 and 2.41 disagree on log1pf (2.41's is correctly
// rounded), and so does bionic on an Echo, which changed 2 of 998 frames
// there. Each is computed in double and rounded once to float instead: the
// correctly rounded result on any libm, identical to the reference on glibc
// 2.41 and 2.39 alike (all 963 table entries, 2026-11-18). The vendored
// sources stay byte-identical; this header is ours.
#ifndef EM_MSF_MATH_H
#define EM_MSF_MATH_H
#include <math.h>
static inline float msf_log1pf(float x) { return (float)log1p((double)x); }
static inline float msf_cosf(float x) { return (float)cos((double)x); }
static inline float msf_powf(float x, float y) { return (float)pow((double)x, (double)y); }
#define log1pf msf_log1pf
#define cosf msf_cosf
#define powf msf_powf
#endif
