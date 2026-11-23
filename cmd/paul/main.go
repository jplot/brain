package main

import (
	"fmt"
	"os"
)

var version = "dev"

const usage = `paul : mémoire persistante pour Claude Code

Usage : paul <commande> [options]

  search <mots>                 --subject --since --until --limit
  souvenirs [mots]              --kind --subject --open --history --limit
  session <id>
  day <AAAA-MM-JJ>
  subjects [mots]               l'arbre des sujets et leurs liens
  memo                          --subject
  summarize                     --session --limit --all --force
  forget point|session <id>     --yes
  index
  rebuild
  web
  hook start|prompt|end
  migrate
  sync                          --new-key
`

func main() {
	fmt.Fprint(os.Stderr, usage)
	os.Exit(2)
}
