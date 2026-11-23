package cli

import (
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jplot/brain/internal/hook"
	"github.com/jplot/brain/internal/index"
	"github.com/jplot/brain/internal/lock"
	"github.com/jplot/brain/internal/memo"
	"github.com/jplot/brain/internal/memory"
	"github.com/jplot/brain/internal/search"
	"github.com/jplot/brain/internal/subjects"
	"github.com/jplot/brain/internal/summary"
	"github.com/jplot/brain/internal/web"
)

var kinds = []string{"topic", "decision", "pending", "fact"}

func pointLine(p memory.Point) string {
	subject := "-"
	if p.SubjectName != nil {
		subject = flat(*p.SubjectName)
	}
	line := fmt.Sprintf("%d  %s  %s  %s", p.ID, p.Kind, subject, flat(p.Text))
	if p.ClosedAt != nil {
		line += "  [clos]"
	}
	return line
}

func markUsed(conn *sql.DB, points []memory.Point) error {
	uids := make([]string, len(points))
	for i, p := range points {
		uids[i] = p.UID
	}
	return memory.MarkUsed(conn, uids, time.Now())
}

func subjectNames(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	flatNames := make([]string, len(names))
	for i, n := range names {
		flatNames[i] = flat(n)
	}
	return strings.Join(flatNames, ", ")
}

func resolveSubject(conn *sql.DB, term string) (subjects.Subject, error) {
	found, err := subjects.FindByTerm(conn, term)
	if err != nil {
		return subjects.Subject{}, err
	}
	switch len(found) {
	case 0:
		return subjects.Subject{}, &subjects.ErrUnknownSubject{Term: term}
	case 1:
		return found[0], nil
	}
	return subjects.Subject{}, &subjects.ErrAmbiguousSubject{Term: term, Candidates: found}
}

func searchCmd(e *env, args []string) (int, error) {
	fs := newFlags("search")
	subject := fs.String("subject", "", "")
	since, until, limit := fs.String("since", "", ""), fs.String("until", "", ""), fs.String("limit", "", "")
	words, err := parseArgs(fs, args)
	if err != nil {
		return 0, err
	}
	if len(words) == 0 {
		return 0, usagef("search : mots de recherche manquants")
	}
	from, err := parseDay(*since)
	if err != nil {
		return 0, err
	}
	to, err := parseDay(*until)
	if err != nil {
		return 0, err
	}
	n, err := count(*limit, 20)
	if err != nil {
		return 0, err
	}
	conn, err := openFresh(e)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	text := strings.Join(words, " ")
	hits, err := search.Search(conn, text, search.Filter{Subject: *subject, Since: from, Until: to, Limit: n})
	if err != nil {
		return 0, err
	}
	points, err := memory.VisiblePoints(conn, memory.PointFilter{Text: text, Subject: *subject,
		Since: *since, Until: *until, Limit: n})
	if err != nil {
		return 0, err
	}
	if err := markUsed(conn, points); err != nil {
		return 0, err
	}
	if len(points) > 0 {
		fmt.Fprintln(e.out, "Souvenirs :")
		for _, p := range points {
			fmt.Fprintln(e.out, pointLine(p))
		}
	}
	for _, h := range hits {
		var b strings.Builder
		for _, s := range h.Segments {
			if s.Match {
				b.WriteString("«" + s.Text + "»")
			} else {
				b.WriteString(s.Text)
			}
		}
		fmt.Fprintf(e.out, "%s  %s  %s  %s  %s\n", local(h.TS), subjectNames(h.Subjects), short(h.SessionID), h.Role, flat(b.String()))
	}
	if len(hits) == 0 && len(points) == 0 {
		fmt.Fprintln(e.out, "Aucun résultat.")
	}
	return 0, nil
}

func souvenirs(e *env, args []string) (int, error) {
	fs := newFlags("souvenirs")
	kind, subject, limit := fs.String("kind", "", ""), fs.String("subject", "", ""), fs.String("limit", "", "")
	openOnly, history := fs.Bool("open", false, ""), fs.Bool("history", false, "")
	words, err := parseArgs(fs, args)
	if err != nil {
		return 0, err
	}
	if *kind != "" && !slices.Contains(kinds, *kind) {
		return 0, usagef("type invalide : %s (attendu %s)", *kind, strings.Join(kinds, ", "))
	}
	n, err := count(*limit, 20)
	if err != nil {
		return 0, err
	}
	conn, err := openFresh(e)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if *subject != "" {
		s, err := resolveSubject(conn, *subject)
		if err != nil {
			return 0, err
		}
		synthesis, err := memory.Synthesis(conn, s.UID)
		if err != nil {
			return 0, err
		}
		if synthesis != "" {
			fmt.Fprintf(e.out, "Synthèse : %s\n", flat(synthesis))
		}
	}
	points, err := memory.VisiblePoints(conn, memory.PointFilter{Kind: *kind, Subject: *subject,
		Text: strings.Join(words, " "), OpenOnly: *openOnly, History: *history, Limit: n})
	if err != nil {
		return 0, err
	}
	if err := markUsed(conn, points); err != nil {
		return 0, err
	}
	for _, p := range points {
		fmt.Fprintln(e.out, pointLine(p))
	}
	if len(points) == 0 {
		fmt.Fprintln(e.out, "Aucun souvenir.")
	}
	return 0, nil
}

func session(e *env, args []string) (int, error) {
	words, err := parseArgs(newFlags("session"), args)
	if err != nil {
		return 0, err
	}
	if len(words) != 1 {
		return 0, usagef("session : un identifiant attendu")
	}
	conn, err := openFresh(e)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	row, messages, err := search.SessionView(conn, words[0])
	if err != nil {
		return 0, err
	}
	if row == nil {
		fmt.Fprintln(e.out, "Session introuvable.")
		return 0, nil
	}
	title := "(sans titre)"
	if row.Title != nil && *row.Title != "" {
		title = flat(*row.Title)
	}
	fmt.Fprintln(e.out, title)
	episodes, err := memory.VisibleEpisodes(conn, row.ID)
	if err != nil {
		return 0, err
	}
	for _, ep := range episodes {
		if ep.Title != nil {
			fmt.Fprintf(e.out, "Résumé : %s [%s]\n", flat(*ep.Title), subjectNames(ep.Subjects))
		}
		if ep.Summary != nil && *ep.Summary != "" {
			fmt.Fprintf(e.out, "  %s\n", flat(*ep.Summary))
		}
		points, err := memory.VisiblePoints(conn, memory.PointFilter{EpisodeID: ep.ID, Limit: allPoints})
		if err != nil {
			return 0, err
		}
		for _, p := range points {
			fmt.Fprintf(e.out, "  %s\n", pointLine(p))
		}
	}
	for _, m := range messages {
		fmt.Fprintf(e.out, "\n%s  %s\n%s\n", local(m.TS), m.Role, m.Text)
	}
	return 0, nil
}

func day(e *env, args []string) (int, error) {
	words, err := parseArgs(newFlags("day"), args)
	if err != nil {
		return 0, err
	}
	if len(words) != 1 {
		return 0, usagef("day : une date AAAA-MM-JJ attendue")
	}
	when, err := parseDay(words[0])
	if err != nil {
		return 0, err
	}
	conn, err := openFresh(e)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	rows, err := search.DayView(conn, *when)
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		title := "(sans titre)"
		if r.Title != nil && *r.Title != "" {
			title = flat(*r.Title)
		}
		fmt.Fprintf(e.out, "%s  %s  %s  %s  %s  (%d messages)\n", local(r.StartedAt), local(r.EndedAt)[11:],
			short(r.ID), subjectNames(r.Subjects), title, r.Messages)
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.out, "Aucune session ce jour.")
	}
	return 0, nil
}

func subjectsTree(e *env, args []string) (int, error) {
	words, err := parseArgs(newFlags("subjects"), args)
	if err != nil {
		return 0, err
	}
	conn, err := openFresh(e)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	tree, err := subjects.Tree(conn)
	if err != nil {
		return 0, err
	}
	names := map[string]string{}
	var collect func(nodes []subjects.Node)
	collect = func(nodes []subjects.Node) {
		for _, n := range nodes {
			names[n.UID] = n.Name
			collect(n.Children)
		}
	}
	collect(tree)
	if len(words) > 0 {
		ids, err := subjects.TreeByTerm(conn, strings.Join(words, " "))
		if err != nil {
			return 0, err
		}
		tree = findNode(tree, ids[0])
	}
	if len(tree) == 0 {
		fmt.Fprintln(e.out, "Aucun sujet.")
		return 0, nil
	}
	var print func(nodes []subjects.Node, depth int)
	print = func(nodes []subjects.Node, depth int) {
		indent := strings.Repeat("  ", depth)
		for _, n := range nodes {
			line := indent + flat(n.Name)
			if n.Kind == "repo" {
				line += "  (repo)"
			}
			if len(n.Aliases) > 0 {
				line += "  [" + subjectNames(n.Aliases) + "]"
			}
			fmt.Fprintln(e.out, line)
			for _, l := range n.Links {
				target := names[l.Target]
				if target == "" {
					target = l.Target
				}
				fmt.Fprintf(e.out, "%s  %s %s\n", indent, l.Type, flat(target))
			}
			print(n.Children, depth+1)
		}
	}
	print(tree, 0)
	return 0, nil
}

func findNode(nodes []subjects.Node, uid string) []subjects.Node {
	for _, n := range nodes {
		if n.UID == uid {
			return []subjects.Node{n}
		}
		if found := findNode(n.Children, uid); found != nil {
			return found
		}
	}
	return nil
}

func memoCmd(e *env, args []string) (int, error) {
	fs := newFlags("memo")
	subject := fs.String("subject", "", "")
	if _, err := parseArgs(fs, args); err != nil {
		return 0, err
	}
	conn, err := openFresh(e)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var text string
	if *subject != "" {
		s, err := resolveSubject(conn, *subject)
		if err != nil {
			return 0, err
		}
		text, err = memo.Index(conn, s.UID, nil)
		if err != nil {
			return 0, err
		}
	} else {
		cwd, err := os.Getwd()
		if err == nil {
			_, err = os.Stat(cwd)
		}
		if err != nil {
			return 0, usagef("le dossier courant n'existe plus")
		}
		if text, err = memo.Preview(conn, cwd, time.Now()); err != nil {
			return 0, err
		}
	}
	if text == "" {
		text = "Aucun souvenir pour ce dossier."
	}
	fmt.Fprintln(e.out, text)
	return 0, nil
}

var statuses = map[string]string{
	summary.Summarized: "résumé", summary.Empty: "vide", summary.TooLong: "trop long",
	summary.Failed: "échec", summary.Already: "déjà résumé", summary.Busy: "base occupée",
	summary.Refreshed: "rafraîchi", summary.Synthesized: "synthèse",
}

func summarize(e *env, args []string) (int, error) {
	fs := newFlags("summarize")
	sessionRef, limit := fs.String("session", "", ""), fs.String("limit", "", "")
	all, force := fs.Bool("all", false, ""), fs.Bool("force", false, "")
	if _, err := parseArgs(fs, args); err != nil {
		return 0, err
	}
	if *force && *sessionRef == "" {
		return 0, usagef("summarize : --force exige --session")
	}
	n, err := count(*limit, 0)
	if err != nil {
		return 0, err
	}
	if _, err := index.Run(e.p); err != nil {
		return 0, err
	}
	id := *sessionRef
	if id != "" {
		conn, err := open(e)
		if err != nil {
			return 0, err
		}
		resolved, err := search.ResolveSession(conn, id)
		conn.Close()
		if err != nil {
			return 0, err
		}
		if resolved != "" {
			id = resolved
		}
	}
	outcomes, ran, err := summary.RunCatchup(e.p, summary.Options{SessionID: id, Limit: n, All: *all, Force: *force})
	if err != nil {
		return 0, err
	}
	if !ran {
		fmt.Fprintln(e.out, "Un résumé est déjà en cours.")
		return 0, nil
	}
	if len(outcomes) == 0 {
		fmt.Fprintln(e.out, "Aucune session à résumer.")
		return 0, nil
	}
	var points int
	var cost float64
	code := 0
	for _, o := range outcomes {
		unit, who := "résumés", ""
		switch {
		case o.SessionID != "":
			unit, who = "points", short(o.SessionID)+"  "
			points += o.Points
		case o.SubjectUID != "":
			unit, who = "points", "sujet "+short(o.SubjectUID)+"  "
		}
		fmt.Fprintf(e.out, "%s%s  %d %s  $%.4f\n", who, statuses[o.Status], o.Points, unit, o.CostUSD)
		cost += o.CostUSD
		if o.Status == summary.Failed {
			code = 1
		}
	}
	fmt.Fprintf(e.out, "Total : %d points, $%.4f\n", points, cost)
	return code, nil
}

func forget(e *env, args []string) (int, error) {
	fs := newFlags("forget")
	yes := fs.Bool("yes", false, "")
	words, err := parseArgs(fs, args)
	if err != nil {
		return 0, err
	}
	if len(words) != 2 || (words[0] != "point" && words[0] != "session") {
		return 0, usagef("forget : forget point|session <id>")
	}
	var pointID int64
	if words[0] == "point" {
		n, err := strconv.ParseInt(words[1], 10, 64)
		if err != nil || n < 1 {
			return 0, usagef("nombre invalide : %s", words[1])
		}
		pointID = n
	}
	conn, err := openFresh(e)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var description, note string
	var erase func() error
	if pointID != 0 {
		found, err := memory.VisiblePoints(conn, memory.PointFilter{PointID: pointID, History: true})
		if err != nil {
			return 0, err
		}
		if len(found) == 0 {
			return 0, &memory.ErrPointNotFound{ID: pointID}
		}
		description = "le point " + pointLine(found[0])
		erase = func() error { return memory.ForgetPoint(conn, pointID) }
	} else {
		row, _, err := search.SessionView(conn, words[1])
		if err != nil {
			return 0, err
		}
		if row == nil {
			return 0, usagef("session introuvable : %s", words[1])
		}
		title := "(sans titre)"
		if row.Title != nil && *row.Title != "" {
			title = flat(*row.Title)
		}
		var messages, episodes, points int
		if err := conn.QueryRow("SELECT (SELECT count(*) FROM messages WHERE session_id = ?1), (SELECT count(*) FROM episodes WHERE session_id = ?1),"+
			" (SELECT count(*) FROM points WHERE episode_id IN (SELECT id FROM episodes WHERE session_id = ?1))", row.ID).Scan(&messages, &episodes, &points); err != nil {
			return 0, err
		}
		description = fmt.Sprintf("la session %s  %s (%d messages, %d épisodes, %d points)", short(row.ID), title, messages, episodes, points)
		note = "\nLe fichier de transcript reste sur le disque : l'oubli ne fait que le retirer de la mémoire."
		erase = func() error {
			release, _, err := lock.Writer(e.p.IndexLock(), true)
			if err != nil {
				return err
			}
			defer release()
			return memory.ForgetSession(conn, row.ID)
		}
	}
	if !*yes {
		fmt.Fprintf(e.out, "Effacerait de la mémoire %s.%s\nRelancer avec --yes pour confirmer.\n", description, note)
		return 2, nil
	}
	if err := erase(); err != nil {
		return 0, err
	}
	fmt.Fprintf(e.out, "Oublié : %s.%s\n", description, note)
	return 0, nil
}

func report(e *env, stats index.Stats) int {
	fmt.Fprintf(e.out, "fichiers : %d\nmessages : %d\nlignes mal formées : %d\nfichiers en échec : %d\n",
		stats.Files, stats.Messages, stats.Malformed, stats.Failed)
	if stats.Failed > 0 {
		return 1
	}
	return 0
}

func indexCmd(e *env, args []string) (int, error) {
	if _, err := parseArgs(newFlags("index"), args); err != nil {
		return 0, err
	}
	stats, err := index.Run(e.p)
	if err != nil {
		return 0, err
	}
	return report(e, stats), nil
}

func rebuild(e *env, args []string) (int, error) {
	if _, err := parseArgs(newFlags("rebuild"), args); err != nil {
		return 0, err
	}
	stats, err := index.Rebuild(e.p)
	if err != nil {
		return 0, err
	}
	return report(e, stats), nil
}

func webCmd(e *env, args []string) (int, error) {
	fs := newFlags("web")
	noOpen := fs.Bool("no-open", false, "")
	if _, err := parseArgs(fs, args); err != nil {
		return 0, err
	}
	return 0, web.Serve(e.p, !*noOpen, e.out)
}

func hookCmd(e *env, args []string) (int, error) {
	if len(args) != 1 {
		return 0, usagef("hook : attendu start, prompt ou end")
	}
	// A closed stdout must give EPIPE, not kill the hook with SIGPIPE.
	signal.Ignore(syscall.SIGPIPE)
	hook.Version = Version
	return hook.Run(args[0], e.p, os.Stdin, e.out), nil
}
