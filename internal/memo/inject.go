package memo

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/memory"
	"github.com/jplot/brain/internal/subjects"
)

const (
	maxSubjects = 3
	maxRun      = 6
)

// Runs of 6 words: a subject name of 6 words or fewer must match whole.
func messageTerms(prompt string) []string {
	seen := map[string]bool{}
	var terms []string
	add := func(term string) {
		if !seen[term] {
			seen[term] = true
			terms = append(terms, term)
		}
	}
	words := strings.Fields(subjects.Normalize(prompt))
	for i, w := range words {
		if utf8.RuneCountInString(w) >= 3 {
			add(w)
		}
		for n := 2; n <= maxRun && i+n <= len(words); n++ {
			add(strings.Join(words[i:i+n], " "))
		}
	}
	return terms
}

type ambiguity struct {
	term       string
	candidates []subjects.Subject
}

func Inject(q db.Querier, sessionID, prompt string) (string, error) {
	terms := messageTerms(prompt)
	if len(terms) == 0 {
		return "", nil
	}
	words := slices.DeleteFunc(slices.Clone(terms), func(t string) bool { return strings.Contains(t, " ") })
	hits, err := subjects.ByTerms(q, terms)
	if err != nil {
		return "", err
	}
	var named []subjects.Subject
	chosen := map[string]bool{}
	var doubts []ambiguity
	for _, term := range terms {
		switch found := hits[term]; len(found) {
		case 0:
		case 1:
			if !chosen[found[0].UID] {
				chosen[found[0].UID] = true
				named = append(named, found[0])
			}
		default:
			doubts = append(doubts, ambiguity{term, found})
		}
	}
	var out []string
	for _, d := range doubts {
		// "mira dupont" already named one of the subjects "mira" could mean.
		if len(out) == maxSubjects || slices.ContainsFunc(d.candidates, func(s subjects.Subject) bool { return chosen[s.UID] }) {
			continue
		}
		key := "ambiguous:" + d.term
		said, err := wasInjected(q, sessionID, key)
		if err != nil {
			return "", err
		}
		if said {
			continue
		}
		if _, err := q.ExecContext(background, "INSERT OR IGNORE INTO injected(session_id, subject_uid) VALUES (?, ?)", sessionID, key); err != nil {
			return "", err
		}
		names := make([]string, len(d.candidates))
		for i, c := range d.candidates {
			names[i] = c.Name
		}
		out = append(out, fmt.Sprintf("Plusieurs sujets portent le nom « %s » : %s. Précise, ou cherche avec paul souvenirs.", d.term, strings.Join(names, ", ")))
	}
	now := time.Now()
	shown := 0
	for _, s := range named {
		if shown == maxSubjects {
			break
		}
		done, err := wasInjected(q, sessionID, s.UID)
		if err != nil {
			return "", err
		}
		if done {
			continue
		}
		text, used, err := index(q, s.UID, words, now)
		if err != nil {
			return "", err
		}
		if _, err := q.ExecContext(background, "INSERT OR IGNORE INTO injected(session_id, subject_uid) VALUES (?, ?)", sessionID, s.UID); err != nil {
			return "", err
		}
		if err := memory.MarkUsed(q, used, now); err != nil {
			return "", err
		}
		out = append(out, text)
		shown++
	}
	return strings.Join(out, "\n"), nil
}

func wasInjected(q db.Querier, sessionID, key string) (bool, error) {
	var n int
	err := q.QueryRowContext(background, "SELECT count(*) FROM injected WHERE session_id = ? AND subject_uid = ?", sessionID, key).Scan(&n)
	return n > 0, err
}
