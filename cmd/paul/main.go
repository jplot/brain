package main

import (
	"fmt"
	"os"
)

var version = "dev"

const usage = `paul : mémoire persistante pour Claude Code

Usage : paul <commande> [options]

  search <mots>                 --project --label --since --until --limit --everywhere
  souvenirs [mots]              --kind --project --label --open --limit --everywhere
  session <id>
  day <AAAA-MM-JJ>
  projects                      --everywhere
  label <projet> personal|work|unsorted
  access <lecteur> <cible>      --revoke --both-ways
  memo                          --project --everywhere
  summarize                     --session --limit --all --force
  forget point|session <id>     --yes --everywhere
  index
  rebuild
  web
  hook start|end
  migrate
  sync                          --new-key
`

func main() {
	fmt.Fprint(os.Stderr, usage)
	os.Exit(2)
}
