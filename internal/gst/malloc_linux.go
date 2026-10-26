package gst

// glibc tuning. The H.264 decoders spread their frame buffers over
// many threads, and Go calls into GStreamer from whichever OS thread it
// likes, so glibc keeps creating per-thread malloc arenas (up to 8 per core)
// and freed memory stays stranded in them: a viewer's RSS crept ~0.5 MB/min
// for over an hour on a 16-core host. A few arenas and a periodic trim keep
// it flat. MALLOC_ARENA_MAX in the environment still wins.

/*
#include <stdlib.h>
#if defined(__GLIBC__)
#include <malloc.h>
#endif

static void __attribute__((constructor)) blite_tune_malloc(void) {
#if defined(__GLIBC__)
	if (getenv("MALLOC_ARENA_MAX") == NULL) mallopt(M_ARENA_MAX, 4);
#endif
}

// In-use and free heap bytes (arenas + mmapped), -1 without glibc 2.33+.
static void blite_mallinfo(long long *inuse, long long *freeb) {
#if defined(__GLIBC__) && (__GLIBC__ > 2 || __GLIBC_MINOR__ >= 33)
	struct mallinfo2 mi = mallinfo2();
	*inuse = (long long)(mi.uordblks + mi.hblkhd);
	*freeb = (long long)mi.fordblks;
#else
	*inuse = -1; *freeb = -1;
#endif
}

static void blite_malloc_trim(void) {
#if defined(__GLIBC__)
	malloc_trim(0);
#endif
}
*/
import "C"

import (
	"sync"
	"time"
)

var trimOnce sync.Once

// startTrimmer hands freed heap pages back to the OS once a minute.
func startTrimmer() {
	trimOnce.Do(func() {
		go func() {
			for range time.Tick(time.Minute) {
				C.blite_malloc_trim()
			}
		}()
	})
}

// MallocStats returns the C heap's in-use and free bytes (-1 if unknown).
func MallocStats() (inuse, free int64) {
	var u, f C.longlong
	C.blite_mallinfo(&u, &f)
	return int64(u), int64(f)
}
