//go:build !windows

package camera

// CheckPrivacy: there are no camera privacy switches outside Windows;
// device permissions show up as Denied errors instead.
func CheckPrivacy() Privacy { return Privacy{} }
