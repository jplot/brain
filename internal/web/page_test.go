package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/jplot/brain/internal/memo"
	"github.com/jplot/brain/internal/subjects"
)

var chrome struct {
	once   sync.Once
	ctx    context.Context
	cancel func()
	err    error
}

func closeBrowser() {
	if chrome.cancel != nil {
		chrome.cancel()
	}
}

// A profile of its own, removed at the end: never the profile of the person running the tests.
func browser(t *testing.T) context.Context {
	t.Helper()
	chrome.once.Do(func() {
		dir, err := os.MkdirTemp("", "paul-chrome")
		if err != nil {
			chrome.err = err
			return
		}
		options := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.UserDataDir(dir))
		if os.Getenv("CI") != "" {
			options = append(options, chromedp.NoSandbox)
		}
		alloc, stopAlloc := chromedp.NewExecAllocator(context.Background(), options...)
		ctx, stopBrowser := chromedp.NewContext(alloc)
		chrome.ctx, chrome.err = ctx, chromedp.Do(ctx)
		chrome.cancel = func() {
			stopBrowser()
			stopAlloc()
			os.RemoveAll(dir)
		}
	})
	if errors.Is(chrome.err, exec.ErrNotFound) {
		t.Skip("Chrome introuvable : test de page sauté")
	}
	if chrome.err != nil {
		t.Fatal(chrome.err)
	}
	return chrome.ctx
}

type page struct {
	t   *testing.T
	f   *fixture
	ctx context.Context

	mu       sync.Mutex
	requests []string
}

// A click on the menu before the end of the boot was redrawn away by the boot's own first view.
func (f *fixture) open(t *testing.T) *page {
	t.Helper()
	pg := f.openAt(t, "/?token="+f.s.token, 1280, 900)
	pg.ready()
	return pg
}

func (pg *page) ready() {
	pg.t.Helper()
	pg.until("première vue", `() => document.querySelector("main h1") !== null`)
}

func (f *fixture) openAt(t *testing.T, path string, width, height int64) *page {
	t.Helper()
	ctx, cancel := chromedp.NewContext(browser(t))
	t.Cleanup(cancel)
	pg := &page{t: t, f: f, ctx: ctx}
	if err := chromedp.Listen(ctx, func(ev any) {
		if e, ok := ev.(*network.EventRequestWillBeSent); ok {
			pg.mu.Lock()
			pg.requests = append(pg.requests, e.Request.URL)
			pg.mu.Unlock()
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := chromedp.Call(ctx, emulation.SetTimezoneOverride, emulation.SetTimezoneOverrideParams{TimezoneID: "Europe/Paris"}); err != nil {
		t.Fatal(err)
	}
	pg.do(chromedp.EmulateViewport(width, height), chromedp.Navigate("http://"+f.s.host+path))
	return pg
}

func (pg *page) do(steps ...chromedp.Action[chromedp.Void]) {
	pg.t.Helper()
	ctx, cancel := context.WithTimeout(pg.ctx, 15*time.Second)
	defer cancel()
	if err := chromedp.Do(ctx, steps...); err != nil {
		pg.t.Fatal(err)
	}
}

func eval[T any](pg *page, script string, args ...any) T {
	pg.t.Helper()
	ctx, cancel := context.WithTimeout(pg.ctx, 15*time.Second)
	defer cancel()
	v, err := chromedp.Run(ctx, chromedp.Evaluate[T](expression(script, args...)))
	if err != nil {
		pg.t.Fatalf("%v\n%s", err, script)
	}
	return v
}

// Arguments go through JSON so test values never need escaping by hand.
func expression(script string, args ...any) string {
	raw := make([]string, len(args))
	for i, a := range args {
		b, _ := json.Marshal(a)
		raw[i] = string(b)
	}
	return "(() => {" + helpers + "return (" + script + ")(" + strings.Join(raw, ", ") + ");})()"
}

const helpers = `
	const scope = (card) => card === "" ? [document] : [...document.querySelectorAll("article")].filter((a) => a.textContent.includes(card));
	const named = (root, name) => [...root.querySelectorAll("button")].find((b) => (b.getAttribute("aria-label") ?? b.textContent.trim()) === name);
`

func (pg *page) until(what, script string, args ...any) {
	pg.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !eval[bool](pg, script, args...) {
		if time.Now().After(deadline) {
			pg.t.Fatalf("attendu : %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (pg *page) never(what, script string, args ...any) {
	pg.t.Helper()
	time.Sleep(400 * time.Millisecond)
	if eval[bool](pg, script, args...) {
		pg.t.Fatalf("inattendu : %s", what)
	}
}

// card is a text the article holds, "" for the whole page; name is the aria-label, else the text.
func (pg *page) press(card, name string) {
	pg.t.Helper()
	pg.until("bouton "+name+" dans "+card, `(card, name) => {
		for (const root of scope(card)) {
			const b = named(root, name);
			if (b) { b.click(); return true; }
		}
		return false;
	}`, card, name)
}

func (pg *page) menu(name string) {
	pg.t.Helper()
	pg.until("menu "+name, `(name) => {
		const b = [...document.querySelectorAll("#menu button")].find((b) => b.firstChild.textContent === name);
		if (b) b.click();
		return !!b;
	}`, name)
}

func (pg *page) count(selector string, n int) {
	pg.t.Helper()
	pg.until(fmt.Sprintf("%d × %s", n, selector), `(sel, n) => document.querySelectorAll(sel).length === n`, selector, n)
}

func (pg *page) has(selector, text string) {
	pg.t.Helper()
	pg.until(fmt.Sprintf("%q dans %s", text, selector), `(sel, text) => [...document.querySelectorAll(sel)].some((e) => e.textContent.includes(text) && e.offsetParent !== null)`, selector, text)
}

func (pg *page) lacks(selector, text string) {
	pg.t.Helper()
	pg.until(fmt.Sprintf("%q absent de %s", text, selector), `(sel, text) => ![...document.querySelectorAll(sel)].some((e) => e.textContent.includes(text))`, selector, text)
}

func (pg *page) fill(selector, value string) {
	pg.t.Helper()
	pg.until("champ "+selector, `(sel, value) => {
		const el = document.querySelector(sel);
		if (!el) return false;
		el.value = value;
		el.dispatchEvent(new Event(el.tagName === "SELECT" ? "change" : "input", { bubbles: true }));
		return true;
	}`, selector, value)
}

func (pg *page) pressed(card, name string, want bool) {
	pg.t.Helper()
	pg.until(fmt.Sprintf("%s pressé = %v", name, want), `(card, name, want) => scope(card).some((r) => {
		const b = named(r, name);
		return b && b.getAttribute("aria-pressed") === String(want);
	})`, card, name, want)
}

func (pg *page) reload() {
	pg.t.Helper()
	pg.do(chromedp.Reload())
	pg.ready()
}

func (pg *page) unfold(card string) {
	pg.t.Helper()
	pg.until("session "+card, `(card) => {
		const b = scope(card).map((r) => r.querySelector && r.querySelector("button.session-toggle")).find(Boolean);
		if (b) b.click();
		return !!b;
	}`, card)
}

func (pg *page) seen(part string) []string {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	var out []string
	for _, u := range pg.requests {
		if strings.Contains(u, part) {
			out = append(out, u)
		}
	}
	return out
}

func (pg *page) sent(part string, n int) {
	pg.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); len(pg.seen(part)) < n; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			pg.t.Fatalf("attendu : %d requêtes %s", n, part)
		}
	}
}

func (pg *page) fitsThePhone() {
	pg.t.Helper()
	if !eval[bool](pg, `() => document.documentElement.scrollWidth <= innerWidth`) {
		pg.t.Fatal("défilement horizontal")
	}
}

func TestPageHostileContentStaysText(t *testing.T) {
	f := newFixture(t)
	evil := `<img src=x onerror="window.pwned=1">`
	f.say("sess-evil", ts, "<script>window.pwned=1</script>"+evil)
	uid := f.subject("<b>evil" + evil)
	episode := f.episode(episodeRow{session: "sess-evil", title: "titre " + evil, summary: "résumé " + evil, subjects: []string{uid}})
	f.point(episode, uid, "pending", "point "+evil, 3)
	pg := f.open(t)
	pg.has("article .summary", "résumé "+evil)
	pg.has("article .title", "titre "+evil)
	pg.unfold("titre " + evil)
	pg.has(".message-text", "<script>window.pwned=1</script>")
	pg.menu("Les souvenirs")
	pg.has("article", "point "+evil)
	pg.menu("Les sujets")
	pg.has("article .subject-name", "<b>evil")
	pg.menu("Le mémo")
	pg.fill(`[data-key="memo-subject"]`, uid)
	pg.has("pre", "point <img")
	if eval[bool](pg, `() => window.pwned !== undefined || document.querySelectorAll("main img, main b, main script").length > 0`) {
		t.Fatal("du HTML a été interprété")
	}
}

func TestPageLoadsNothingExternal(t *testing.T) {
	f := newFixture(t)
	f.indexed()
	pg := f.open(t)
	pg.count("article", 2)
	all := pg.seen("")
	if len(all) < 3 {
		t.Fatal(all)
	}
	for _, u := range all {
		if !strings.HasPrefix(u, "http://"+f.s.host+"/") {
			t.Fatal(u)
		}
	}
}

func TestPageIsFrenchAndTitled(t *testing.T) {
	f := newFixture(t)
	pg := f.open(t)
	pg.has(".tagline", "Sur cette machine uniquement.")
	if got := eval[string](pg, `() => document.title + "|" + document.documentElement.lang`); got != "paul|fr" {
		t.Fatal(got)
	}
}

func TestPageWithoutOrWithWrongTokenShowsNothing(t *testing.T) {
	f := newFixture(t)
	f.indexed()
	pg := f.openAt(t, "/", 1280, 900)
	pg.until("refus affiché", `() => document.body.textContent.includes("refusé")`)
	pg = f.openAt(t, "/?token=mauvais", 1280, 900)
	pg.until("erreur affichée", `() => document.body.textContent.includes("error")`)
	if eval[bool](pg, `() => document.body.textContent.includes("mot de A") || document.querySelectorAll("article").length > 0`) {
		t.Fatal("la page s'affiche sans jeton")
	}
}

func TestPageRefreshesTheIndexOnLoad(t *testing.T) {
	f := newFixture(t)
	f.transcript("late", userLine("late", ts, "arrivé après le démarrage"))
	f.open(t).count("article", 1)
}

func TestFeedGroupsByDay(t *testing.T) {
	f := newFixture(t)
	f.say("s1", ts, "question", claudeLine("s1", "2026-10-20T10:05:00.000Z", []obj{{"type": "text", "text": "réponse"}}))
	pg := f.open(t)
	pg.has("h2", "Mardi 20 octobre")
	pg.has("article", "12:05")
	pg.has("article", "(sans titre)")
	pg.has("article", "2 messages")
}

func TestFeedUnfoldsMessagesWithToolsInMonospace(t *testing.T) {
	f := newFixture(t)
	f.say("s1", ts, "question", claudeLine("s1", "2026-10-20T10:01:00.000Z",
		[]obj{{"type": "tool_use", "id": "t", "name": "Read", "input": obj{"file_path": "/x/f.go"}}}))
	pg := f.open(t)
	pg.unfold("(sans titre)")
	pg.has(".message-text", "question")
	if got := eval[string](pg, `() => getComputedStyle(document.querySelector(".message-text.tool")).fontFamily`); !strings.Contains(got, "monospace") {
		t.Fatal(got)
	}
	if eval[string](pg, `() => document.querySelector("article button").getAttribute("aria-expanded")`) != "true" {
		t.Fatal()
	}
	pg.unfold("(sans titre)")
	pg.count(".message", 0)
}

func TestFeedFiltersBySubject(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	pg := f.open(t)
	pg.count("article", 2)
	pg.fill(`[data-key="subject-filter"]`, w.a)
	pg.count("article", 1)
	pg.has("article", "Titre A")
	pg.fill(`[data-key="subject-filter"]`, "")
	pg.count("article", 2)
}

func (f *fixture) manySessions(n int, at func(i int) string) {
	f.t.Helper()
	for i := range n {
		id := fmt.Sprintf("s%02d", i)
		f.transcript(id, userLine(id, at(i), fmt.Sprintf("message %d", i)))
	}
	f.index()
}

func TestFeedSeeMoreWithoutDuplicate(t *testing.T) {
	f := newFixture(t)
	f.manySessions(25, func(i int) string {
		return fmt.Sprintf("2026-10-20T10:%02d:00.000Z", min(i, 17))
	})
	pg := f.open(t)
	pg.count("article", 20)
	eval[bool](pg, `() => { const b = document.querySelector('[data-key="more"]'); b.click(); b.click(); return true; }`)
	pg.count("article", 25)
	pg.count(`[data-key="more"]`, 0)
	pg.never("plus de 25 cartes", `() => document.querySelectorAll("article").length !== 25`)
	eval[bool](pg, `() => { document.querySelectorAll("article button[aria-expanded]").forEach((b) => b.click()); return true; }`)
	pg.count(".message-text", 25)
	texts := eval[[]string](pg, `() => [...document.querySelectorAll(".message-text")].map((e) => e.textContent)`)
	seen := map[string]bool{}
	for _, text := range texts {
		seen[text] = true
	}
	if len(seen) != 25 {
		t.Fatal(texts)
	}
}

func TestFeedDayAcrossTwoPages(t *testing.T) {
	f := newFixture(t)
	f.manySessions(25, func(i int) string {
		if i < 12 {
			return fmt.Sprintf("2026-10-21T10:%02d:00.000Z", i)
		}
		return fmt.Sprintf("2026-10-20T10:%02d:00.000Z", i)
	})
	pg := f.open(t)
	pg.count("article", 20)
	pg.press("", "Voir plus")
	pg.count("article", 25)
	got := eval[string](pg, `() => [...document.querySelectorAll("h2")].map((e) => e.textContent).join("|") + "/" +
		[...document.querySelectorAll(".days .stack")].map((s) => s.children.length).join(",")`)
	if got != "Mercredi 21 octobre|Mardi 20 octobre/12,13" {
		t.Fatal(got)
	}
}

func TestFeedErrorShowsAMessage(t *testing.T) {
	f := newFixture(t)
	f.indexed()
	f.fail("/api/sessions", 500)
	pg := f.open(t)
	pg.has("[role=alert]", "Impossible de charger les sessions.")
	pg.has("h1", "Le fil")
}

func TestSearchHighlightsAndWaitsForTheTyping(t *testing.T) {
	f := newFixture(t)
	f.indexed()
	pg := f.open(t)
	pg.menu("La recherche")
	pg.has(".result-count", "Tape un mot pour chercher.")
	eval[bool](pg, `() => {
		const input = document.querySelector('[data-key="query"]');
		for (const text of ["m", "mo", "mot", "mot ", "mot d", "mot de"]) {
			input.value = text;
			input.dispatchEvent(new Event("input", { bubbles: true }));
		}
		return true;
	}`)
	pg.count("mark", 4)
	pg.has(".result-count", "2 résultats")
	if got := pg.seen("/api/search"); len(got) != 1 {
		t.Fatal(got)
	}
	pg.fill(`[data-key="query"]`, "introuvable")
	pg.has(".result-count", "Aucun résultat.")
}

func TestSearchFiltersBySubject(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	pg := f.open(t)
	pg.menu("La recherche")
	pg.fill(`[data-key="query"]`, "mot")
	pg.count("mark", 2)
	pg.fill(`[data-key="subject-filter"]`, w.b)
	pg.fill(`[data-key="query"]`, "mot")
	pg.count("mark", 1)
	pg.has(".result-count", "1 résultat")
	pg.has(".hit", "Beta")
}

func TestButtonsLargeEnoughAndFocusVisible(t *testing.T) {
	f := newFixture(t)
	f.memories()
	pg := f.open(t)
	pg.count("article", 2)
	small := `() => [...document.querySelectorAll("button, select")].filter((b) => b.offsetParent !== null && b.offsetHeight < 44).map((b) => b.textContent)`
	for _, view := range []string{"", "Les souvenirs", "Les sujets", "Le mémo"} {
		if view != "" {
			pg.menu(view)
			pg.has("h1", view)
		}
		if view == "Les sujets" {
			pg.press("Alpha", "Lier")
		}
		if got := eval[[]string](pg, small); len(got) != 0 {
			t.Fatal(view, got)
		}
	}
	pg.do(chromedp.KeyEvent(kb.Tab))
	if got := eval[string](pg, `() => getComputedStyle(document.activeElement).outlineStyle`); got == "none" {
		t.Fatal(got)
	}
}

func TestPhoneWidth(t *testing.T) {
	f := newFixture(t)
	f.memories()
	f.subject(strings.Repeat("x", 60))
	pg := f.openAt(t, "/?token="+f.s.token, 390, 844)
	pg.ready()
	pg.unfold("Titre A")
	pg.press("", "Oublier cette session")
	pg.fitsThePhone()
	pg.menu("Les souvenirs")
	pg.press("Équipe de six", "Oublier")
	pg.press("Équipe de six", "Corriger")
	pg.fitsThePhone()
	pg.menu("Les sujets")
	pg.press("Alpha", "Fusionner")
	pg.press(strings.Repeat("x", 60), "Lier")
	pg.fitsThePhone()
	pg.menu("Le mémo")
	pg.has("pre", "Mémoire de tes sessions précédentes")
	pg.fitsThePhone()
}

func TestMemoriesListAndKindFilter(t *testing.T) {
	f := newFixture(t)
	f.memories()
	pg := f.open(t)
	pg.menu("Les souvenirs")
	pg.count("article", 3)
	if eval[string](pg, `() => [...document.querySelectorAll("#menu button")].find((b) => b.textContent.startsWith("Les souvenirs")).getAttribute("aria-current")`) != "page" {
		t.Fatal()
	}
	for _, tab := range []struct{ name, text string }{{"Décisions", "Une facture est unique"}, {"En suspens", "Écrire la clé"}, {"Faits sur toi", "Équipe de six"}} {
		pg.press("", tab.name)
		pg.count("article", 1)
		pg.has("article", tab.text)
	}
	pg.press("", "Tout")
	pg.count("article", 3)
}

func TestImportanceAndTextCorrectionSurviveAReload(t *testing.T) {
	f := newFixture(t)
	f.memories()
	pg := f.open(t)
	pg.menu("Les souvenirs")
	pg.press("Équipe de six", "Importance 5")
	pg.pressed("Équipe de six", "Importance 5", true)
	pg.press("Équipe de six", "Corriger")
	pg.fill(`.edit-field input`, "Équipe de sept développeurs")
	pg.press("", "Terminer")
	pg.has("article", "Équipe de sept")
	pg.reload()
	pg.menu("Les souvenirs")
	pg.pressed("Équipe de sept", "Importance 5", true)
	pg.has("article", "Corrigé par toi")
	pg.lacks("main", "Équipe de six")
}

func TestCorrectionMasksASecret(t *testing.T) {
	f := newFixture(t)
	f.memories()
	secret := "ghp_" + strings.Repeat("A1", 15)
	pg := f.open(t)
	pg.menu("Les souvenirs")
	pg.press("Équipe de six", "Corriger")
	pg.fill(`.edit-field input`, "clé "+secret)
	pg.press("", "Terminer")
	pg.has("article", "[secret masqué]")
	pg.lacks("body", secret)
}

func TestRefusedCorrectionKeepsTheStateAndTheDraft(t *testing.T) {
	f := newFixture(t)
	f.memories()
	f.fail("/api/points/correct", 400)
	pg := f.open(t)
	pg.menu("Les souvenirs")
	pg.press("Équipe de six", "Importance 5")
	pg.has("[role=alert]", "Changement refusé : panne 400")
	pg.pressed("Équipe de six", "Importance 2", true)
	pg.press("Équipe de six", "Corriger")
	pg.fill(`.edit-field input`, "Nouveau texte")
	pg.press("", "Terminer")
	pg.has("[role=alert]", "panne 400")
	if got := eval[string](pg, `() => document.querySelector(".edit-field input").value`); got != "Nouveau texte" {
		t.Fatal(got)
	}
	sent := len(pg.seen("/api/points/correct"))
	pg.press("Fait sur toi", "Importance 2")
	time.Sleep(300 * time.Millisecond)
	if got := len(pg.seen("/api/points/correct")); got != sent {
		t.Fatal("l'importance déjà choisie a été envoyée")
	}
}

func TestClosePendingPoint(t *testing.T) {
	f := newFixture(t)
	f.memories()
	pg := f.open(t)
	pg.menu("Les souvenirs")
	pg.press("Écrire la clé", "Fermer le point")
	pg.has("article.closed", "Fermé")
	if eval[int](pg, `() => [...document.querySelectorAll("button")].filter((b) => b.textContent === "Fermer le point").length`) != 0 {
		t.Fatal()
	}
	pg.reload()
	pg.menu("Les souvenirs")
	pg.has("article.closed", "Écrire la clé")
}

func TestSeeTheConversationOfAMemory(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	f.point(f.episode(episodeRow{session: "fantome", title: "Fantôme"}), w.a, "fact", "Souvenir orphelin", 3)
	pg := f.open(t)
	pg.menu("Les souvenirs")
	pg.press("Équipe de six", "Voir la conversation")
	pg.has("h1", "Le fil")
	pg.has(".message-text", "mot de B")
	pg.fill(`[data-key="subject-filter"]`, w.a)
	pg.fill(`[data-key="subject-filter"]`, "")
	pg.count("article", 2)
	pg.lacks(".pinned", "mot de B")
	pg.menu("Les souvenirs")
	pg.press("Souvenir orphelin", "Voir la conversation")
	pg.has("[role=alert]", "Cette conversation n’existe plus.")
	f.fail("/api/sessions/sess-B", 503)
	pg.menu("Les souvenirs")
	pg.press("Équipe de six", "Voir la conversation")
	pg.has("[role=alert]", "Impossible de charger la conversation.")
}

func TestForgetAPointWithConfirmation(t *testing.T) {
	f := newFixture(t)
	f.memories()
	pg := f.open(t)
	pg.has("article", "Résumé B")
	pg.menu("Les souvenirs")
	pg.press("Équipe de six", "Oublier")
	pg.press("Équipe de six", "Annuler")
	pg.press("Équipe de six", "Oublier")
	pg.has("article", "Définitif.")
	eval[bool](pg, `() => { scope("Équipe de six").map((r) => named(r, "Oublier"))[0].dispatchEvent(new MouseEvent("click", { detail: 2, bubbles: true })); return true; }`)
	pg.never("un oubli sur un double clic", `() => document.querySelectorAll("article").length !== 3`)
	if got := pg.seen("forget"); len(got) != 0 {
		t.Fatal(got)
	}
	pg.press("Équipe de six", "Oublier")
	pg.count("article", 2)
	pg.has("#menu", "Les souvenirs2")
	pg.reload()
	pg.menu("Les souvenirs")
	pg.count("article", 2)
	pg.menu("Le fil")
	pg.count("article", 2)
	pg.lacks("main", "Résumé B")
	pg.lacks("main", "Titre B")
}

func TestForgetAPointAlreadyGoneOrRefused(t *testing.T) {
	f := newFixture(t)
	f.memories()
	f.fail("/api/points/forget", 503)
	pg := f.open(t)
	pg.menu("Les souvenirs")
	pg.press("Équipe de six", "Oublier")
	pg.press("Équipe de six", "Oublier")
	pg.has("article", "Oubli refusé : panne 503")
	pg.count("article", 3)
	f.fail("/api/points/forget", 404)
	f.fail("/api/points", 500)
	pg.press("Équipe de six", "Oublier")
	pg.count("article", 2)
	pg.lacks("main", "Équipe de six")
}

func TestForgetASessionWithConfirmation(t *testing.T) {
	f := newFixture(t)
	f.memories()
	pg := f.open(t)
	pg.unfold("Titre A")
	pg.has(".message-text", "mot de A")
	pg.press("Titre A", "Oublier cette session")
	pg.has("article", "Oublier toute cette session ? C’est définitif.")
	pg.press("Titre A", "Annuler")
	pg.press("Titre A", "Oublier cette session")
	eval[bool](pg, `() => { scope("Titre A").map((r) => named(r, "Oublier"))[0].dispatchEvent(new MouseEvent("click", { detail: 2, bubbles: true })); return true; }`)
	pg.never("un oubli sur un double clic", `() => document.querySelectorAll("article").length !== 2`)
	pg.press("Titre A", "Oublier")
	pg.count("article", 1)
	pg.lacks("main", "mot de A")
	pg.has("#menu", "Le fil1")
	pg.has("#menu", "Les souvenirs1")
	pg.reload()
	pg.count("article", 1)
	pg.menu("Les souvenirs")
	pg.count("article", 1)
}

func TestForgetSessionRefusedIsShown(t *testing.T) {
	f := newFixture(t)
	f.memories()
	f.fail("/api/sessions/forget", 503)
	pg := f.open(t)
	pg.unfold("Titre A")
	pg.press("Titre A", "Oublier cette session")
	pg.press("Titre A", "Oublier")
	pg.has("article", "Oubli refusé : panne 503")
	pg.count("article", 2)
}

func TestForgetSessionRemovesOnlyItsCard(t *testing.T) {
	f := newFixture(t)
	f.manySessions(25, func(i int) string { return fmt.Sprintf("2026-10-20T10:%02d:00.000Z", i) })
	pg := f.open(t)
	pg.press("", "Voir plus")
	pg.count("article", 25)
	before := len(pg.seen("/api/sessions?"))
	eval[bool](pg, `() => { document.querySelectorAll("article")[3].querySelector("button").click(); return true; }`)
	pg.press("message 21", "Oublier cette session")
	pg.press("message 21", "Oublier")
	pg.count("article", 24)
	if got := len(pg.seen("/api/sessions?")); got != before {
		t.Fatal(got, before)
	}
}

func TestServerUnreachableGivesAFrenchMessage(t *testing.T) {
	f := newFixture(t)
	f.memories()
	pg := f.open(t)
	pg.menu("Les souvenirs")
	f.abort("/api/points/close")
	pg.press("Écrire la clé", "Fermer le point")
	pg.has("[role=alert]", "Le serveur ne répond pas")
	pg.lacks("[role=alert]", "Failed")
}

func TestMemoriesPagination(t *testing.T) {
	f := newFixture(t)
	w := f.indexed()
	episode := f.episode(episodeRow{session: "sess-A", title: "t", subjects: []string{w.a}})
	for n := range 61 {
		f.point(episode, w.a, "fact", fmt.Sprintf("souvenir %02d", n), 5-n%5)
	}
	pg := f.open(t)
	pg.menu("Les souvenirs")
	pg.count("article", 50)
	pg.has(".result-count", "61 souvenirs")
	eval[bool](pg, `() => { const b = document.querySelector('[data-key="more"]'); b.click(); b.click(); return true; }`)
	pg.count("article", 61)
	pg.count(`[data-key="more"]`, 0)
	pg.press("souvenir 60", "Importance 1")
	pg.press("souvenir 60", "Importance 5")
	pg.pressed("souvenir 60", "Importance 5", true)
	pg.count("article", 61)
	texts := eval[[]string](pg, `() => [...document.querySelectorAll(".memory > div:nth-child(2)")].map((e) => e.textContent)`)
	seen := map[string]bool{}
	for _, text := range texts {
		seen[text] = true
	}
	if len(seen) != 61 {
		t.Fatal(len(seen))
	}
}

func TestMemoShowsWhatClaudeReceives(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	pg := f.open(t)
	pg.menu("Le mémo")
	pg.has("pre", "Dernières sessions :")
	pg.fill(`[data-key="memo-subject"]`, w.a)
	pg.has("pre", "Sujet « Alpha »")
	want, err := memo.Index(f.conn, w.a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := eval[string](pg, `() => document.querySelector("pre").textContent`); got != want {
		t.Fatalf("%q\n%q", got, want)
	}
	f.fail("/api/memo", 500)
	pg.fill(`[data-key="memo-subject"]`, w.b)
	pg.has("[role=alert]", "Impossible de charger le mémo.")
	if eval[bool](pg, `() => document.querySelector("pre").offsetParent !== null`) {
		t.Fatal("l'ancien mémo reste affiché")
	}
}

func TestEmptyMemoSaysSo(t *testing.T) {
	f := newFixture(t)
	pg := f.open(t)
	pg.menu("Le mémo")
	pg.has("main", "Rien à retenir ici : Claude ne recevrait aucun mémo.")
}

func TestSubjectsTreeWithLinksAndSynthesis(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	house := f.subject("Maison", "domicile")
	if err := subjects.SetParent(f.conn, w.a, house, subjects.Origin{}); err != nil {
		t.Fatal(err)
	}
	if err := subjects.Link(f.conn, w.a, "depends_on", w.b, subjects.Origin{}); err != nil {
		t.Fatal(err)
	}
	f.exec("INSERT INTO syntheses(subject_uid, uid, text, created_at, updated_at) VALUES (?, 'y1', 'Tout sur Alpha.', ?, ?)", w.a, ts, ts)
	pg := f.open(t)
	pg.has("#menu", "Les sujets3")
	pg.menu("Les sujets")
	pg.has(`.node .children article[aria-label="Alpha"]`, "dépend de Beta")
	pg.has(`article[aria-label="Alpha"]`, "Tout sur Alpha.")
	pg.has(`article[aria-label="Alpha"]`, "2 souvenirs")
	pg.has(`article[aria-label="Maison"]`, "Mots-clés : domicile")
	pg.press("Alpha", "Voir les souvenirs")
	pg.has("h1", "Les souvenirs")
	pg.count("article", 2)
}

func TestRenameASubject(t *testing.T) {
	f := newFixture(t)
	w := f.indexed()
	pg := f.open(t)
	pg.menu("Les sujets")
	pg.press("Alpha", "Renommer")
	pg.fill(`[data-key="name-`+w.a+`"]`, "Atelier")
	pg.press("", "Enregistrer")
	pg.count(`article[aria-label="Atelier"]`, 1)
	if s, _ := subjects.Get(f.conn, w.a); s.Name != "Atelier" {
		t.Fatal(s.Name)
	}
	f.fail("/api/subjects/rename", 400)
	pg.press("Atelier", "Renommer")
	pg.fill(`[data-key="name-`+w.a+`"]`, "Refusé")
	pg.press("", "Enregistrer")
	pg.has("[role=alert]", "Changement refusé : panne 400")
	pg.count(`article[aria-label="Atelier"]`, 1)
	if got := eval[string](pg, `(sel) => document.querySelector(sel).value`, `[data-key="name-`+w.a+`"]`); got != "Refusé" {
		t.Fatal(got)
	}
}

func TestMergeTwoSubjects(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	pg := f.open(t)
	pg.menu("Les sujets")
	pg.press("Beta", "Fusionner")
	pg.fill(`[data-key="into-`+w.b+`"]`, w.a)
	pg.has(`article[aria-label="Beta"]`, "C’est définitif.")
	pg.press("Beta", "Confirmer la fusion")
	pg.count(`article[aria-label="Beta"]`, 0)
	pg.has(`article[aria-label="Alpha"]`, "Mots-clés : Beta")
	pg.has(`article[aria-label="Alpha"]`, "3 souvenirs")
	pg.has("#menu", "Les sujets1")
	if _, err := subjects.Get(f.conn, w.b); err != subjects.ErrNotFound {
		t.Fatal(err)
	}
}

func TestMoveLinkAndUnlinkSubjects(t *testing.T) {
	f := newFixture(t)
	w := f.indexed()
	pg := f.open(t)
	pg.menu("Les sujets")
	pg.press("Alpha", "Déplacer")
	if got := eval[[]string](pg, `(sel) => [...document.querySelector(sel).options].map((o) => o.textContent)`, `[data-key="parent-`+w.a+`"]`); strings.Join(got, "|") != "Aucun : à la racine|Beta" {
		t.Fatal(got)
	}
	pg.fill(`[data-key="parent-`+w.a+`"]`, w.b)
	pg.press("Alpha", "Déplacer ici")
	pg.count(`.children article[aria-label="Alpha"]`, 1)
	if f.count("SELECT count(*) FROM subjects WHERE uid = ? AND parent_uid = ? AND parent_origin = 'user'", w.a, w.b) != 1 {
		t.Fatal()
	}
	pg.press("Beta", "Lier")
	pg.fill(`[data-key="link-type-`+w.b+`"]`, "about")
	pg.fill(`[data-key="link-target-`+w.b+`"]`, w.a)
	pg.press("Beta", "Ajouter le lien")
	pg.has(`article[aria-label="Beta"]`, "concerne Alpha")
	pg.press("Beta", "Retirer le lien vers Alpha")
	pg.lacks(`article[aria-label="Beta"]`, "concerne Alpha")
	if f.count("SELECT count(*) FROM links WHERE source_uid = ? AND target_uid = ? AND active = 0 AND origin = 'user'", w.b, w.a) != 1 {
		t.Fatal()
	}
}

func TestFeedShowsSummaryAndCounters(t *testing.T) {
	f := newFixture(t)
	f.memories()
	f.say("sess-C", ts, "mot de C")
	f.episode(episodeRow{session: "sess-C", summary: "Orphelin"})
	f.episode(episodeRow{session: "sess-C", title: "Périmé", summary: "Résumé périmé", stale: true, start: "2026-10-20T11:00:00.000Z"})
	pg := f.open(t)
	pg.has("article", "Résumé A")
	pg.has("article", "1 décision, 1 point en suspens")
	pg.has("article", "(sans titre)")
	pg.has("article", "1 message")
	pg.lacks("main", "Orphelin")
	pg.lacks("main", "Périmé")
}

func TestSearchIgnoresAnAnswerOvertaken(t *testing.T) {
	f := newFixture(t)
	f.indexed()
	pg := f.open(t)
	pg.menu("La recherche")
	pg.has(".result-count", "Tape un mot pour chercher.")
	release := f.hold("/api/search")
	pg.fill(`[data-key="query"]`, "mot")
	pg.sent("/api/search", 1)
	pg.fill(`[data-key="query"]`, "mot de A")
	pg.has(".result-count", "1 résultat")
	release()
	pg.never("la réponse dépassée affichée", `() => document.querySelectorAll(".hit").length !== 1`)
	release = f.hold("/api/search")
	pg.fill(`[data-key="query"]`, "mot")
	pg.sent("/api/search", 3)
	pg.fill(`[data-key="query"]`, "")
	pg.has(".result-count", "Tape un mot pour chercher.")
	release()
	pg.never("une réponse affichée sur un champ vide", `() => document.querySelectorAll(".hit").length !== 0`)
}

func TestForgetFromTheConversationOfAMemory(t *testing.T) {
	f := newFixture(t)
	f.memories()
	pg := f.open(t)
	pg.menu("Les souvenirs")
	pg.press("Équipe de six", "Voir la conversation")
	pg.has(".pinned .message-text", "mot de B")
	pg.press("mot de B", "Oublier cette session")
	pg.press("mot de B", "Oublier")
	pg.lacks("main", "mot de B")
	pg.count("article", 1)
}

func TestSeeMoreAfterForgettingTheOnlyCardOfADay(t *testing.T) {
	f := newFixture(t)
	f.manySessions(24, func(i int) string {
		if i < 19 {
			return fmt.Sprintf("2026-10-20T12:%02d:00.000Z", 59-i)
		}
		return fmt.Sprintf("2026-10-17T12:%02d:00.000Z", 78-i)
	})
	pg := f.open(t)
	pg.count("article", 20)
	eval[bool](pg, `() => { [...document.querySelectorAll("article")].at(-1).querySelector("button").click(); return true; }`)
	pg.press("message 19", "Oublier cette session")
	pg.press("message 19", "Oublier")
	pg.count("article", 19)
	pg.press("", "Voir plus")
	pg.count("article", 23)
	pg.has("h2", "Samedi 17 octobre")
}

func TestForgettingClearsTheDrafts(t *testing.T) {
	f := newFixture(t)
	f.memories()
	pg := f.open(t)
	drafts := `() => Object.keys(memoryUi.draft).length + Object.keys(memoryUi.editing).length`
	pg.menu("Les souvenirs")
	pg.press("Équipe de six", "Corriger")
	pg.fill(".edit-field input", "brouillon")
	pg.press("Fait sur toi", "Oublier")
	pg.press("Fait sur toi", "Oublier")
	pg.count("article", 2)
	if n := eval[int](pg, drafts); n != 0 {
		t.Fatal(n)
	}
	pg.press("Écrire la clé", "Corriger")
	pg.fill(".edit-field input", "brouillon")
	pg.menu("Le fil")
	pg.unfold("Titre A")
	pg.press("Titre A", "Oublier cette session")
	pg.press("Titre A", "Oublier")
	pg.count("article", 1)
	if n := eval[int](pg, drafts); n != 0 {
		t.Fatal(n)
	}
}
