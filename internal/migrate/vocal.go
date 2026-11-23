package migrate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jplot/brain/internal/db"
)

const vocalTitle = "Conversation vocale"

type journalLine struct {
	V       int    `json:"v"`
	ID      string `json:"id"`
	Session string `json:"session"`
	TS      string `json:"ts"`
	Role    string `json:"role"`
	Text    string `json:"text"`
	Title   string `json:"title,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
}

// The file is created on the first line only: an empty journal is never linked in place.
type journal struct {
	path string
	f    *os.File
	w    *bufio.Writer
	enc  *json.Encoder
	n    int
}

func (j *journal) write(l journalLine) error {
	if j.f == nil {
		if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(j.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		j.f, j.w = f, bufio.NewWriter(f)
		j.enc = json.NewEncoder(j.w)
		j.enc.SetEscapeHTML(false)
	}
	j.n++
	return j.enc.Encode(l)
}

func (j *journal) close() error {
	if j.f == nil {
		return nil
	}
	return errors.Join(j.w.Flush(), j.f.Close())
}

// The session ids are the old version's: its episodes and forgotten sessions name them.
func convertVocal(from, dir string) (int, error) {
	in, err := os.Open(from)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer in.Close()
	j := &journal{path: filepath.Join(dir, "vocal.jsonl")}
	defer j.close()
	lines := bufio.NewReader(in)
	for {
		raw, err := lines.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		// A last line without its newline is still being written: the old version skipped it too.
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			break
		}
		if l, ok := vocalLine(raw); ok {
			if err := j.write(l); err != nil {
				return 0, err
			}
		}
	}
	return j.n, j.close()
}

func vocalLine(raw []byte) (journalLine, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if dec.Decode(&obj) != nil {
		return journalLine{}, false
	}
	number, ok := obj["t"].(json.Number)
	if !ok {
		return journalLine{}, false
	}
	t, err := number.Float64()
	if err != nil || math.IsInf(t, 0) || math.IsNaN(t) {
		return journalLine{}, false
	}
	kind, _ := obj["type"].(string)
	role, said := spoken(obj, kind)
	text, _ := said.(string)
	if text = strings.TrimSpace(text); text == "" {
		return journalLine{}, false
	}
	at := moment(t)
	session, _ := obj["session"].(string)
	if session == "" {
		session = "vocal-" + at.Local().Format("2006-01-02")
	}
	return journalLine{V: 1, ID: number.String() + ":" + kind, Session: session, TS: db.Stamp(at), Role: role, Text: text, Title: vocalTitle}, true
}

func spoken(obj map[string]any, kind string) (role string, text any) {
	switch kind {
	case "toi":
		return "user", obj["texte"]
	case "agent":
		return "subagent", obj["resultat"]
	case "reponse":
		said, _ := obj["reponse"].(map[string]any)
		if obj["annule"] != true && said != nil && said["action"] != "deleguer" {
			return "claude", said["texte"]
		}
	}
	return "", nil
}

// Python's fromtimestamp rounds half-even to the microsecond: 1700000000.9999996 is
// 22:13:21.000, not 22:13:20.999, and the episodes' end_ts were taken from it.
func moment(t float64) time.Time {
	whole, frac := math.Modf(t)
	us := math.RoundToEven(frac * 1e6)
	switch {
	case us >= 1e6:
		whole, us = whole+1, us-1e6
	case us < 0:
		whole, us = whole-1, us+1e6
	}
	return time.Unix(int64(whole), int64(us)*1000)
}
