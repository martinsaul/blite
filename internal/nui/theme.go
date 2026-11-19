package nui

import (
	"fmt"
	"hash/fnv"
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// Palette for the dark UI with one indigo/periwinkle accent (#6C7BFF).
var (
	colBg         = color.NRGBA{R: 0x0E, G: 0x10, B: 0x16, A: 0xFF}
	colCardBg     = color.NRGBA{R: 0x16, G: 0x19, B: 0x23, A: 0xFF}
	colCardBorder = color.NRGBA{R: 0x26, G: 0x2C, B: 0x3D, A: 0xFF}
	colElevated   = color.NRGBA{R: 0x1A, G: 0x1E, B: 0x2B, A: 0xFF}
	colButton     = color.NRGBA{R: 0x20, G: 0x25, B: 0x35, A: 0xFF}
	colButtonDis  = color.NRGBA{R: 0x16, G: 0x19, B: 0x24, A: 0xFF}
	colInputBg    = color.NRGBA{R: 0x12, G: 0x15, B: 0x1F, A: 0xFF}
	colInputBord  = color.NRGBA{R: 0x2C, G: 0x33, B: 0x47, A: 0xFF}
	colAccent     = color.NRGBA{R: 0x6C, G: 0x7B, B: 0xFF, A: 0xFF}
	colAccentFill = color.NRGBA{R: 0x6C, G: 0x7B, B: 0xFF, A: 0x30}
	colRowHover   = color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0x0D}
	colText       = color.NRGBA{R: 0xEE, G: 0xF0, B: 0xF7, A: 0xFF}
	colTextDim    = color.NRGBA{R: 0x8B, G: 0x93, B: 0xA8, A: 0xFF}
	colTextDis    = color.NRGBA{R: 0x56, G: 0x5E, B: 0x73, A: 0xFF}
	colSeparator  = color.NRGBA{R: 0x23, G: 0x28, B: 0x38, A: 0xFF}
	colDanger     = color.NRGBA{R: 0xE0, G: 0x48, B: 0x48, A: 0xFF}
	colSuccess    = color.NRGBA{R: 0x3C, G: 0xCB, B: 0x7F, A: 0xFF}
	colSuccessBg  = color.NRGBA{R: 0x3C, G: 0xCB, B: 0x7F, A: 0x22}
	colWarning    = color.NRGBA{R: 0xF0, G: 0xAD, B: 0x4E, A: 0xFF}
	colWarningBg  = color.NRGBA{R: 0xF0, G: 0xAD, B: 0x4E, A: 0x22}
	colWhite      = color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
)

var avatarPalette = []color.NRGBA{
	{R: 0x5B, G: 0x6A, B: 0xF0, A: 0xFF}, // indigo
	{R: 0x2A, G: 0x9D, B: 0x8F, A: 0xFF}, // teal
	{R: 0xD9, G: 0x53, B: 0x6F, A: 0xFF}, // rose
	{R: 0xC8, G: 0x7D, B: 0x28, A: 0xFF}, // amber
	{R: 0x35, G: 0xA4, B: 0x6C, A: 0xFF}, // emerald
	{R: 0x8B, G: 0x5C, B: 0xF6, A: 0xFF}, // violet
	{R: 0x2E, G: 0x8B, B: 0xC0, A: 0xFF}, // sky
	{R: 0xD9, G: 0x62, B: 0x48, A: 0xFF}, // coral
}

func avatarColor(name string) color.NRGBA {
	h := fnv.New32a()
	h.Write([]byte(name))
	return avatarPalette[h.Sum32()%uint32(len(avatarPalette))]
}

func svgResource(name, hex, pathD string) fyne.Resource {
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="24" height="24"><path fill="%s" d="%s"/></svg>`, hex, pathD)
	return fyne.NewStaticResource(name, []byte(svg))
}

const (
	eyeOpenPath    = "M12 4.5C7 4.5 2.73 7.61 1 12c1.73 4.39 6 7.5 11 7.5s9.27-3.11 11-7.5c-1.73-4.39-6-7.5-11-7.5zM12 17c-2.76 0-5-2.24-5-5s2.24-5 5-5 5 2.24 5 5-2.24 5-5 5zm0-8c-1.66 0-3 1.34-3 3s1.34 3 3 3 3-1.34 3-3-1.34-3-3-3z"
	eyeClosedPath  = "M12 7c2.76 0 5 2.24 5 5 0 .65-.13 1.26-.36 1.83l2.92 2.92c1.51-1.26 2.7-2.89 3.43-4.75-1.73-4.39-6-7.5-11-7.5-1.4 0-2.74.25-3.98.7l2.16 2.16C10.74 7.13 11.35 7 12 7zM2 4.27l2.28 2.28.46.46C3.08 8.3 1.78 10.02 1 12c1.73 4.39 6 7.5 11 7.5 1.55 0 3.03-.3 4.38-.84l.42.42L19.73 22 21 20.73 3.27 3 2 4.27zM7.53 9.8l1.55 1.55c-.05.21-.08.43-.08.65 0 1.66 1.34 3 3 3 .22 0 .44-.03.65-.08l1.55 1.55c-.67.33-1.41.53-2.2.53-2.76 0-5-2.24-5-5 0-.79.2-1.53.53-2.2zm4.31-.78l3.15 3.15.02-.16c0-1.66-1.34-3-3-3l-.17.01z"
	lockClosedPath = "M18 8h-1V6c0-2.76-2.24-5-5-5S7 3.24 7 6v2H6c-1.1 0-2 .9-2 2v10c0 1.1.9 2 2 2h12c1.1 0 2-.9 2-2V10c0-1.1-.9-2-2-2zm-6 9c-1.1 0-2-.9-2-2s.9-2 2-2 2 .9 2 2-.9 2-2 2zm3.1-9H8.9V6c0-1.71 1.39-3.1 3.1-3.1 1.71 0 3.1 1.39 3.1 3.1v2z"
	lockOpenPath   = "M12 17c1.1 0 2-.9 2-2s-.9-2-2-2-2 .9-2 2 .9 2 2 2zm6-9h-1V6c0-2.76-2.24-5-5-5S7 3.24 7 6h1.9c0-1.71 1.39-3.1 3.1-3.1 1.71 0 3.1 1.39 3.1 3.1v2H6c-1.1 0-2 .9-2 2v10c0 1.1.9 2 2 2h12c1.1 0 2-.9 2-2V10c0-1.1-.9-2-2-2zm0 12H6V10h12v10z"
	screenPath     = "M21 2H3c-1.1 0-2 .9-2 2v12c0 1.1.9 2 2 2h7v2H8v2h8v-2h-2v-2h7c1.1 0 2-.9 2-2V4c0-1.1-.9-2-2-2zm0 14H3V4h18v12z"
	windowPath     = "M19 4H5c-1.11 0-2 .9-2 2v12c0 1.1.89 2 2 2h14c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm0 14H5V8h14v10z"
	soundOnPath    = "M3 9v6h4l5 5V4L7 9H3zm13.5 3c0-1.77-1.02-3.29-2.5-4.03v8.05c1.48-.73 2.5-2.25 2.5-4.02zM14 3.23v2.06c2.89.86 5 3.54 5 6.71s-2.11 5.85-5 6.71v2.06c4.01-.91 7-4.49 7-8.77s-2.99-7.86-7-8.77z"
	soundOffPath   = "M16.5 12c0-1.77-1.02-3.29-2.5-4.03v2.21l2.45 2.45c.03-.2.05-.41.05-.63zm2.5 0c0 .94-.2 1.82-.54 2.64l1.51 1.51C20.63 14.91 21 13.5 21 12c0-4.28-2.99-7.86-7-8.77v2.06c2.89.86 5 3.54 5 6.71zM4.27 3L3 4.27 7.73 9H3v6h4l5 5v-6.73l4.25 4.25c-.67.52-1.42.93-2.25 1.18v2.06c1.38-.31 2.63-.95 3.69-1.81L19.73 21 21 19.73l-9-9L4.27 3zM12 4L9.91 6.09 12 8.18V4z"
	closePath      = "M19 6.41L17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z"
)

var (
	eyeOpenWhiteRes   = svgResource("eye-open-white.svg", "#EEF0F7", eyeOpenPath)
	eyeClosedWhiteRes = svgResource("eye-closed-white.svg", "#EEF0F7", eyeClosedPath)
	eyeGreyRes        = svgResource("eye-grey.svg", "#52596E", eyeOpenPath)
	lockClosedRes     = svgResource("lock-closed.svg", "#3CCB7F", lockClosedPath)
	lockOpenRes       = svgResource("lock-open.svg", "#F0AD4E", lockOpenPath)
	screenWhiteRes    = svgResource("screen-white.svg", "#EEF0F7", screenPath)
	screenAccentRes   = svgResource("screen-accent.svg", "#6C7BFF", screenPath)
	windowWhiteRes    = svgResource("window-white.svg", "#EEF0F7", windowPath)
	windowAccentRes   = svgResource("window-accent.svg", "#6C7BFF", windowPath)
	soundOnRes        = svgResource("sound-on.svg", "#EEF0F7", soundOnPath)
	soundOffRes       = svgResource("sound-off.svg", "#8B93A8", soundOffPath)
	closeDimRes       = svgResource("close-dim.svg", "#8B93A8", closePath)
	closeWhiteRes     = svgResource("close-white.svg", "#EEF0F7", closePath)
)

type bliteTheme struct {
	base fyne.Theme
}

var darkTheme fyne.Theme = &bliteTheme{base: theme.DarkTheme()}

func (t *bliteTheme) Color(name fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameBackground:
		return colBg
	case theme.ColorNameOverlayBackground, theme.ColorNameMenuBackground, theme.ColorNameHeaderBackground:
		return colElevated
	case theme.ColorNameButton:
		return colButton
	case theme.ColorNameDisabledButton:
		return colButtonDis
	case theme.ColorNameInputBackground:
		return colInputBg
	case theme.ColorNameInputBorder:
		return colInputBord
	case theme.ColorNamePrimary, theme.ColorNameHyperlink:
		return colAccent
	case theme.ColorNameFocus, theme.ColorNameSelection:
		return colAccentFill
	case theme.ColorNameHover:
		return colRowHover
	case theme.ColorNameForeground:
		return colText
	case theme.ColorNamePlaceHolder:
		return colTextDim
	case theme.ColorNameDisabled:
		return colTextDis
	case theme.ColorNameSeparator:
		return colSeparator
	case theme.ColorNameError:
		return colDanger
	case theme.ColorNameSuccess:
		return colSuccess
	case theme.ColorNameWarning:
		return colWarning
	case theme.ColorNameForegroundOnPrimary, theme.ColorNameForegroundOnError, theme.ColorNameForegroundOnSuccess:
		return colWhite
	}
	return t.base.Color(name, theme.VariantDark)
}

func (t *bliteTheme) Font(s fyne.TextStyle) fyne.Resource {
	return t.base.Font(s)
}

func (t *bliteTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return t.base.Icon(name)
}

func (t *bliteTheme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNameCardRadius, theme.SizeNameDialogRadius:
		return 12
	case theme.SizeNamePopupRadius, theme.SizeNameMenuRadius:
		return 10
	case theme.SizeNameButtonRadius, theme.SizeNameInputRadius, theme.SizeNameSelectionRadius:
		return 8
	}
	return t.base.Size(name)
}
