package gst

// Xlib's default error handler exits the process. GStreamer's ximagesrc asks
// X about the window it captures on every frame, so a shared window that
// closed killed Blite with "BadWindow (invalid Window parameter)" (e.g.
// sharing the grid window while it was rebuilt). This handler
// logs the error and lets the call fail instead; the capture then stops with
// an error the session reports. libX11 is loaded the same way the plugins
// load it, so the handler is theirs too; without it (Wayland only) nothing
// is done.

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdio.h>

typedef struct {
	int type;
	void *display;
	unsigned long resourceid;
	unsigned long serial;
	unsigned char error_code;
	unsigned char request_code;
	unsigned char minor_code;
} blite_xerror;

static int blite_on_xerror(void *dpy, blite_xerror *e) {
	fprintf(stderr, "X error (ignored): code %d, request %d.%d, resource 0x%lx, serial %lu\n",
		e->error_code, e->request_code, e->minor_code, e->resourceid, e->serial);
	return 0;
}

static int blite_x11_handler(void) {
	void *x = dlopen("libX11.so.6", RTLD_NOW | RTLD_GLOBAL);
	if (!x) return 0;
	void *(*set)(void *) = (void *(*)(void *))dlsym(x, "XSetErrorHandler");
	if (!set) return 0;
	set((void *)blite_on_xerror);
	return 1;
}
*/
import "C"

func setX11ErrorHandler() { C.blite_x11_handler() }
