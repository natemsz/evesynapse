package app

// demoPortraits gives each invented demo character a real pilot's
// portrait of the character's gender, so the demo shows faces where
// EVE has none for its made-up IDs. The pilots are picked from the
// killboard's own kills; only the picture is borrowed, never the name
// or the record.
var demoPortraits = map[int64]int64{
	demoCharacterID:  2123951263, // Sable Iversen
	demoCharacter2ID: 2115162438, // Juno Park

	demoCharMarcus: 1526116574, // Marcus Hale
	demoCharIlya:   497615347,  // Ilya Sorin
	demoCharPetra:  92657625,   // Petra Lindqvist
	demoCharSera:   2124729206, // Sera Voss
	demoCharDario:  92540943,   // Dario Fenn
	demoCharMina:   96932837,   // Mina Okafor
	demoCharKato:   2124508681, // Kato Vire
	demoCharRhen:   647388233,  // Rhen Absalom
	demoCharLio:    91674965,   // Lio Marsh

	demoPilotAiko:   2113956510, // Aiko Renn
	demoPilotBram:   2121111621, // Bram Tollan
	demoPilotCass:   2122362163, // Cass Medina
	demoPilotDov:    601219682,  // Dov Harker
	demoPilotElin:   2119847784, // Elin Sato
	demoPilotFen:    2048293842, // Fen Rourke
	demoPilotGreta:  92102210,   // Greta Holt
	demoPilotHollis: 2120307642, // Hollis Ward
	demoPilotImre:   2115869888, // Imre Castell
	demoPilotJae:    96207282,   // Jae Moreau
	demoPilotKira:   2124540177, // Kira Nolan
	demoPilotLars:   264486129,  // Lars Ebner
	demoPilotMaren:  2115278975, // Maren Cole
	demoPilotNils:   2122062757, // Nils Arden
	demoPilotOda:    2122549707, // Oda Frey
	demoPilotPriya:  2121910450, // Priya Anand
	demoPilotQuinn:  2121058559, // Quinn Adair
	demoPilotRosa:   94403074,   // Rosa Keel
	demoPilotSoren:  2114623409, // Soren Vale
	demoPilotTova:   2124521472, // Tova Lind
	demoPilotIvo:    92533808,   // Ivo Brandt

	demoRivalUlric: 2120435769, // Ulric Stone
	demoRivalVera:  2118995528, // Vera Pike
	demoRivalWes:   2114629983, // Wes Tarrant
	demoRivalXan:   2120474507, // Xan Mirov
	demoRivalYara:  2122445447, // Yara Flint
	demoRivalZeke:  2114157716, // Zeke Harlan
	demoRivalAdda:  2122300421, // Adda Korvin
	demoRivalBoro:  1526116574, // Boro Easling
}

// portraitOf is the character whose portrait stands for id: itself, or
// for an invented demo character, its stand-in.
func portraitOf(id int64) int64 {
	if stand, ok := demoPortraits[id]; ok {
		return stand
	}
	return id
}

// portraitOfAny is portraitURL for templates, whose IDs come in every
// integer width.
func portraitOfAny(id any, size int) string {
	var n int64
	switch v := id.(type) {
	case int64:
		n = v
	case int32:
		n = int64(v)
	case int:
		n = int64(v)
	}
	return portraitURL(n, size)
}
