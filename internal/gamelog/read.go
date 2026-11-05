package gamelog

// Reading a game log whole: an uploaded file, a pasted part of one,
// or the lines Axon sends as it tails the pilot's Gamelogs folder.
// The text is read as a stream, line by line, and only the fights
// built from it are kept.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxLine is the longest line a game log is read with. The client's
// lines are a few hundred bytes of markup; a longer one is not a
// game log.
const MaxLine = 64 << 10

// MaxText is the most text one log unpacks to.
const MaxText = 32 << 20

var (
	// ErrNotGameLog says the text held no game log line at all.
	ErrNotGameLog = errors.New("no game log lines")
	// ErrTooLarge says the text runs past MaxText.
	ErrTooLarge = errors.New("game log over 32 MB")
)

var sessionName = regexp.MustCompile(`^(\d{8})_(\d{6})(?:_(\d+))?\.txt$`)

// Log is what one game log, or part of one, held.
type Log struct {
	Listener    string // the header's Listener line, if the text had one
	CharacterID int64  // from the session file's name, if it has one
	Session     string // the session file's name, if it has one
	Lines       int    // game log lines read
	Combat      int    // of them, combat lines
	Quiet       int    // fights with no player on the other side, not kept
	Fights      []Fight
}

// SessionCharacter is the character ID a session file's name
// carries ("20261104_120000_90000001.txt"), or 0.
func SessionCharacter(name string) int64 {
	m := sessionName.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	id, _ := strconv.ParseInt(m[3], 10, 64)
	return id
}

// Read reads a game log as the client writes it (UTF-8 or UTF-16,
// lines ending in CRLF, LF or a bare CR), gzipped or not, up to
// MaxText, and cuts its combat into fights. session is the file's
// name (a ".gz" suffix is dropped), or "" for pasted text; the
// header block is optional, so a part of a log reads too. The fight
// in progress at the end of the text is closed there.
func Read(r io.Reader, session string) (Log, error) {
	session = strings.TrimSuffix(session, ".gz")
	l := Log{Session: session, CharacterID: SessionCharacter(session)}
	seg := &Segmenter{CharacterID: l.CharacterID, Session: session}
	br := bufio.NewReaderSize(r, 64<<10)
	if head, _ := br.Peek(2); len(head) == 2 && head[0] == 0x1F && head[1] == 0x8B {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return l, ErrNotGameLog
		}
		defer zr.Close()
		br = bufio.NewReaderSize(zr, 64<<10)
	}
	br = bufio.NewReaderSize(&capReader{r: br, left: MaxText}, 64<<10)
	if head, _ := br.Peek(2); len(head) == 2 && head[0] == 0xFF && head[1] == 0xFE {
		_, _ = br.Discard(2)
		r = &utf16Reader{r: br}
	} else {
		r = br
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLine)
	sc.Split(splitLines)
	header := true
	keep := func(f Fight, ok bool) {
		if ok {
			l.Fights = append(l.Fights, f)
		}
	}
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" {
			continue
		}
		if header && !strings.HasPrefix(line, "[") {
			if name, ok := strings.CutPrefix(line, "Listener:"); ok && l.Listener == "" {
				l.Listener = strings.TrimSpace(name)
				seg.Listener = l.Listener
			}
			continue
		}
		header = false
		ev, combat, ok := ParseCombat(line)
		if gameStamp.MatchString(line) {
			l.Lines++
		}
		if !combat {
			continue
		}
		l.Combat++
		if !ok {
			if strings.Contains(line, "(combat)") {
				seg.Unparsed()
			}
			continue
		}
		keep(seg.Add(ev))
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) || errors.Is(err, gzip.ErrChecksum) || errors.Is(err, gzip.ErrHeader) {
			err = ErrNotGameLog
		}
		return l, err
	}
	keep(seg.Flush())
	l.Quiet = seg.Quiet
	if l.Lines == 0 {
		return l, ErrNotGameLog
	}
	return l, nil
}

// capReader fails with ErrTooLarge past left bytes.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		var one [1]byte
		if n, _ := c.r.Read(one[:]); n > 0 {
			return 0, ErrTooLarge
		}
		return c.r.Read(p)
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// splitLines splits on CRLF, LF or a bare CR.
func splitLines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\r' {
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil // a CRLF may be split across reads
			}
			if i+1 < len(data) && data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
		}
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// utf16Reader turns little-endian UTF-16 into UTF-8.
type utf16Reader struct {
	r   *bufio.Reader
	out []byte
	hi  rune // a high surrogate waiting for its pair
}

func (u *utf16Reader) Read(p []byte) (int, error) {
	for len(u.out) == 0 {
		var b [2]byte
		if _, err := io.ReadFull(u.r, b[:]); err != nil {
			if err == io.ErrUnexpectedEOF {
				err = io.EOF
			}
			return 0, err
		}
		c := rune(b[0]) | rune(b[1])<<8
		switch {
		case utf16.IsSurrogate(c) && c < 0xDC00:
			u.hi = c
			continue
		case utf16.IsSurrogate(c) && u.hi != 0:
			c = utf16.DecodeRune(u.hi, c)
		case utf16.IsSurrogate(c):
			c = utf8.RuneError
		}
		u.hi = 0
		u.out = utf8.AppendRune(u.out, c)
	}
	n := copy(p, u.out)
	u.out = u.out[n:]
	return n, nil
}
