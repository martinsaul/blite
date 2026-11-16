package nui

import _ "embed"

// iconPNG is the app icon (window, taskbar, notifications); the tray keeps
// its own small glyph, which stays sharp at 16-32 px.
//
//go:embed icon.png
var iconPNG []byte
