//go:build !windows && !linux

package nui

func getWinGeom(uintptr) (winGeom, bool) { return winGeom{}, false }
func setWinGeom(uintptr, winGeom)        {}
