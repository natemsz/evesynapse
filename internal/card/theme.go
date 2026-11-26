package card

import "image/color"

// Palette carries every colour a rendered card or profile sheet uses.
// Each named palette mirrors its theme tokens in static/bg.css and
// static/style.css; the zero Theme selects the default ember look.
type Palette struct {
	GroundTop  color.NRGBA
	GroundBot  color.NRGBA
	Glow1      color.NRGBA
	Glow2      color.NRGBA
	Panel      color.NRGBA
	Text       color.NRGBA
	TextBright color.NRGBA
	Muted      color.NRGBA
	Muted2     color.NRGBA
	Hi         color.NRGBA
	Accent     color.NRGBA
	Rule0      color.NRGBA
	Rule1      color.NRGBA
	EveWord    color.NRGBA
	SynWords   [7]color.NRGBA
	MarkUp0    color.NRGBA
	MarkUp1    color.NRGBA
	MarkSheen  color.NRGBA
	MarkDn0    color.NRGBA
	MarkDn1    color.NRGBA
	MarkGlow   color.NRGBA
	MarkSpark  color.NRGBA
}

var defaultPalette = Palette{
	GroundTop:  groundTop,
	GroundBot:  groundBot,
	Glow1:      glow1,
	Glow2:      glow2,
	Panel:      panelC,
	Text:       textC,
	TextBright: textBright,
	Muted:      mutedC,
	Muted2:     muted2C,
	Hi:         hiC,
	Accent:     accentC,
	Rule0:      ruleGold,
	Rule1:      ruleEmber,
	EveWord:    hex(0xffffff),
	SynWords: [7]color.NRGBA{
		hex(0xffc36c), hex(0xffa551), hex(0xff8835), hex(0xff6a1a), hex(0xf35d18), hex(0xe85017), hex(0xdc4315),
	},
	MarkUp0:   hex(0xffe2a0),
	MarkUp1:   hex(0xff7a26),
	MarkSheen: hex(0xfff3d6),
	MarkDn0:   hex(0xff6a1a),
	MarkDn1:   hex(0xc8360f),
	MarkGlow:  hex(0xffd27a),
	MarkSpark: hex(0xfff6e6),
}

var namedPalettes = map[string]Palette{
	"minmatar-v3": defaultPalette,
	"jove": {
		GroundTop:  hex(0x170b2c),
		GroundBot:  hex(0x0d0716),
		Glow1:      color.NRGBA{139, 92, 246, 107},
		Glow2:      color.NRGBA{55, 48, 163, 51},
		Panel:      hex(0x1a1620),
		Text:       hex(0xf3effa),
		TextBright: hex(0xfaf7ff),
		Muted:      hex(0xa59bb8),
		Muted2:     hex(0xb3abc4),
		Hi:         hex(0xf472b6),
		Accent:     hex(0x8b5cf6),
		Rule0:      hex(0xf472b6),
		Rule1:      hex(0x3730a3),
		EveWord:    hex(0xffffff),
		SynWords: [7]color.NRGBA{
			hex(0xf472b6), hex(0xd26bca), hex(0xb164df), hex(0x8d5df5), hex(0x8161f0), hex(0x7569eb), hex(0x686ee5),
		},
		MarkUp0:   hex(0xf472b6),
		MarkUp1:   hex(0x8b5cf6),
		MarkSheen: hex(0xfff3fa),
		MarkDn0:   hex(0x8b5cf6),
		MarkDn1:   hex(0x3730a3),
		MarkGlow:  hex(0xf472b6),
		MarkSpark: hex(0xfaf7ff),
	},
	"caldari": {
		GroundTop:  hex(0x0f141c),
		GroundBot:  hex(0x0a0d12),
		Glow1:      color.NRGBA{96, 165, 250, 112},
		Glow2:      color.NRGBA{33, 94, 205, 69},
		Panel:      hex(0x11161f),
		Text:       hex(0xeaf1f8),
		TextBright: hex(0xf5f9fe),
		Muted:      hex(0x93a6bc),
		Muted2:     hex(0xa3b4c8),
		Hi:         hex(0x7dd3fc),
		Accent:     hex(0x60a5fa),
		Rule0:      hex(0x7dd3fc),
		Rule1:      hex(0x263242),
		EveWord:    hex(0xffffff),
		SynWords: [7]color.NRGBA{
			hex(0x7dd3fc), hex(0x69b3fb), hex(0x5896e2), hex(0x4e7eba), hex(0x597eac), hex(0x657e9f), hex(0x6f7d8f),
		},
		MarkUp0:   hex(0x7dd3fc),
		MarkUp1:   hex(0x60a5fa),
		MarkSheen: hex(0xeef8ff),
		MarkDn0:   hex(0x60a5fa),
		MarkDn1:   hex(0x263242),
		MarkGlow:  hex(0x7dd3fc),
		MarkSpark: hex(0xf5f9fe),
	},
	"gallente": {
		GroundTop:  hex(0x07231c),
		GroundBot:  hex(0x04120e),
		Glow1:      color.NRGBA{45, 212, 191, 99},
		Glow2:      color.NRGBA{14, 104, 128, 56},
		Panel:      hex(0x101917),
		Text:       hex(0xe9f5f1),
		TextBright: hex(0xf4fbf8),
		Muted:      hex(0x8ca89f),
		Muted2:     hex(0x9db7af),
		Hi:         hex(0x4ade80),
		Accent:     hex(0x2dd4bf),
		Rule0:      hex(0x4ade80),
		Rule1:      hex(0x155e75),
		EveWord:    hex(0xffffff),
		SynWords: [7]color.NRGBA{
			hex(0x4ade80), hex(0x34d6b1), hex(0x29c2b3), hex(0x24a8a3), hex(0x1f8e94), hex(0x328897), hex(0x43869e),
		},
		MarkUp0:   hex(0x4ade80),
		MarkUp1:   hex(0x2dd4bf),
		MarkSheen: hex(0xe8fff4),
		MarkDn0:   hex(0x2dd4bf),
		MarkDn1:   hex(0x155e75),
		MarkGlow:  hex(0x4ade80),
		MarkSpark: hex(0xf4fbf8),
	},
	"amarr": {
		GroundTop:  hex(0x241a06),
		GroundBot:  hex(0x120c03),
		Glow1:      color.NRGBA{240, 185, 63, 110},
		Glow2:      color.NRGBA{152, 102, 20, 51},
		Panel:      hex(0x16120d),
		Text:       hex(0xfaf3e4),
		TextBright: hex(0xfff9ed),
		Muted:      hex(0xb5a17e),
		Muted2:     hex(0xc2b090),
		Hi:         hex(0xfacc15),
		Accent:     hex(0xf0b93f),
		Rule0:      hex(0xfacc15),
		Rule1:      hex(0x9a3412),
		EveWord:    hex(0xffffff),
		SynWords: [7]color.NRGBA{
			hex(0xfacc15), hex(0xedb43d), hex(0xdc9b35), hex(0xcc822c), hex(0xbc6824), hex(0xc06130), hex(0xc65c3d),
		},
		MarkUp0:   hex(0xfacc15),
		MarkUp1:   hex(0xf0b93f),
		MarkSheen: hex(0xfffbe0),
		MarkDn0:   hex(0xf0b93f),
		MarkDn1:   hex(0x9a3412),
		MarkGlow:  hex(0xfacc15),
		MarkSpark: hex(0xfff9ed),
	},
	"triglavian": {
		GroundTop:  hex(0x260709),
		GroundBot:  hex(0x120405),
		Glow1:      color.NRGBA{172, 22, 10, 107},
		Glow2:      color.NRGBA{128, 18, 10, 61},
		Panel:      hex(0x191112),
		Text:       hex(0xf7ecee),
		TextBright: hex(0xfff5f6),
		Muted:      hex(0xb3989c),
		Muted2:     hex(0xbfa6aa),
		Hi:         hex(0xd41236),
		Accent:     hex(0xd41236),
		Rule0:      hex(0xd41236),
		Rule1:      hex(0x570a04),
		EveWord:    hex(0xffffff),
		SynWords: [7]color.NRGBA{
			hex(0xff6b81), hex(0xec4b60), hex(0xe53248), hex(0xdd413c), hex(0xcf4e3e), hex(0xc35a4b), hex(0xb56255),
		},
		MarkUp0:   hex(0xd41236),
		MarkUp1:   hex(0xac160a),
		MarkSheen: hex(0xffe4e8),
		MarkDn0:   hex(0xac160a),
		MarkDn1:   hex(0x570a04),
		MarkGlow:  hex(0xd41236),
		MarkSpark: hex(0xfff5f6),
	},
	"edencom": {
		GroundTop:  hex(0x18202b),
		GroundBot:  hex(0x0b0f14),
		Glow1:      color.NRGBA{233, 207, 143, 97},
		Glow2:      color.NRGBA{51, 65, 85, 51},
		Panel:      hex(0x191b1e),
		Text:       hex(0xeef1f6),
		TextBright: hex(0xf8fafd),
		Muted:      hex(0x9aa4b4),
		Muted2:     hex(0xaab3c2),
		Hi:         hex(0xeab308),
		Accent:     hex(0xe9cf8f),
		Rule0:      hex(0xeab308),
		Rule1:      hex(0x334155),
		EveWord:    hex(0xffffff),
		SynWords: [7]color.NRGBA{
			hex(0xeab308), hex(0xe9cf8d), hex(0xc2b183), hex(0x9d9477), hex(0x7e7d71), hex(0x777e83), hex(0x6e7d94),
		},
		MarkUp0:   hex(0xeab308),
		MarkUp1:   hex(0xe9cf8f),
		MarkSheen: hex(0xfff9e2),
		MarkDn0:   hex(0xe9cf8f),
		MarkDn1:   hex(0x334155),
		MarkGlow:  hex(0xeab308),
		MarkSpark: hex(0xf8fafd),
	},
	"soe": {
		GroundTop:  hex(0x0c171d),
		GroundBot:  hex(0x070d11),
		Glow1:      color.NRGBA{229, 82, 82, 107},
		Glow2:      color.NRGBA{146, 32, 32, 61},
		Panel:      hex(0x101a20),
		Text:       hex(0xeaf3f6),
		TextBright: hex(0xf4fafc),
		Muted:      hex(0x90a8b2),
		Muted2:     hex(0xa0b6bf),
		Hi:         hex(0xa1dde0),
		Accent:     hex(0xe55252),
		Rule0:      hex(0x57d7e7),
		Rule1:      hex(0x922020),
		EveWord:    hex(0xffffff),
		SynWords: [7]color.NRGBA{
			hex(0x57d7e7), hex(0x7db4c0), hex(0xa1939a), hex(0xc37175), hex(0xdf4e4e), hex(0xd2504c), hex(0xca564f),
		},
		MarkUp0:   hex(0xa1dde0),
		MarkUp1:   hex(0xe55252),
		MarkSheen: hex(0xeafafd),
		MarkDn0:   hex(0xe55252),
		MarkDn1:   hex(0x922020),
		MarkGlow:  hex(0xa1dde0),
		MarkSpark: hex(0xf4fafc),
	},
	"minmatar": {
		GroundTop:  hex(0x17100b),
		GroundBot:  hex(0x0b0806),
		Glow1:      color.NRGBA{255, 106, 26, 107},
		Glow2:      color.NRGBA{122, 58, 18, 51},
		Panel:      hex(0x1a120b),
		Text:       hex(0xfaf3e6),
		TextBright: hex(0xfff6e6),
		Muted:      hex(0xb39a7d),
		Muted2:     hex(0xb8a58c),
		Hi:         hex(0xffd27a),
		Accent:     hex(0xff6a1a),
		Rule0:      hex(0xffc36c),
		Rule1:      hex(0xdc4315),
		EveWord:    hex(0xffffff),
		SynWords: [7]color.NRGBA{
			hex(0xffc36c), hex(0xffa551), hex(0xff8835), hex(0xff6a1a), hex(0xf35d18), hex(0xe85017), hex(0xdc4315),
		},
		MarkUp0:   hex(0xffd27a),
		MarkUp1:   hex(0xff6a1a),
		MarkSheen: hex(0xfff3d6),
		MarkDn0:   hex(0xff6a1a),
		MarkDn1:   hex(0x7a3a12),
		MarkGlow:  hex(0xffd27a),
		MarkSpark: hex(0xfff6e6),
	},
}

// PaletteFor returns the Palette matching a theme or scheme name,
// falling back to the default ember palette when unknown or empty.
func PaletteFor(theme string) Palette {
	if p, ok := namedPalettes[theme]; ok {
		return p
	}
	return defaultPalette
}

// KnownTheme reports whether name is a recognized card theme or scheme.
func KnownTheme(theme string) bool {
	_, ok := namedPalettes[theme]
	return ok
}

var redgreenSecColors = [11]color.NRGBA{
	hex(0xa8609e), hex(0xb4521e), hex(0xd06c22), hex(0xe8922c), hex(0xf2b640),
	hex(0xf6ec7a), hex(0xa8dcf0), hex(0x7cc6f4), hex(0x58acf2), hex(0x3f8eea), hex(0x2f6fdc),
}

var blueyellowSecColors = [11]color.NRGBA{
	hex(0xa848a8), hex(0xc03050), hex(0xe0405a), hex(0xff6a4a), hex(0xff8a5c),
	hex(0xfff0b0), hex(0xb5ecd8), hex(0x7fe0d0), hex(0x4ecef8), hex(0x3a9aeb), hex(0x2c74e0),
}

// SecColorFor returns the security-status colour tuned for a card theme
// and optional colour-vision mode ("redgreen" or "blueyellow").
func SecColorFor(tier int, theme, vision string) color.NRGBA {
	if tier < 0 || tier > 10 {
		return PaletteFor(theme).Text
	}
	switch vision {
	case "redgreen":
		return redgreenSecColors[tier]
	case "blueyellow":
		return blueyellowSecColors[tier]
	}
	return secColors[tier]
}
