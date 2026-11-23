# paul

Mémoire persistante pour Claude Code. paul indexe les conversations, en tire des souvenirs (sujets, décisions, suspens, faits) rattachés à des sujets, et les rapporte à Claude quand on en parle : au démarrage d'une session, puis à chaque message qui touche un sujet connu.

Tout reste sur la machine, dans `~/.claude/paul/`. Les secrets repérés dans les messages sont masqués avant l'indexation.

## Installation

Dans Claude Code :

```
claude plugin marketplace add jplot/brain
claude plugin install paul@jplot
```

Au premier démarrage de session, le plugin télécharge le binaire correspondant à la plateforme (macOS ou Linux, arm64 ou amd64), en vérifie l'empreinte, puis installe la commande `paul` dans `~/.local/bin`. Cet emplacement se change avec `PAUL_BIN_DIR`. Dépendances : `claude`, `git`, `curl`, `shasum` ou `sha256sum`.

Aucun réglage n'est nécessaire. Variables facultatives : `PAUL_HOME` (défaut `~/.claude/paul/`) et `PAUL_TRANSCRIPTS` (défaut `~/.claude/projects/`).

## Synchronisation facultative

Pour retrouver ses souvenirs sur plusieurs machines, créer `~/.claude/paul/config.toml` :

```toml
[sync]
repository = "git@github.com:mon-compte/mes-souvenirs.git"
encryption = "age"   # facultatif
```

Sans cette section, rien n'est synchronisé.

## Commandes

```
paul search <mots>              --subject --since --until --limit
paul souvenirs [mots]           --kind --subject --open --history --limit
paul session <id>
paul day <AAAA-MM-JJ>
paul subjects [mots]            l'arbre des sujets et leurs liens
paul memo                       --subject
paul summarize                  --session --limit --all --force
paul forget point|session <id>  --yes
paul index
paul rebuild
paul web                        --no-open
paul migrate                    --from --root
paul sync                       --new-key
```

- `--kind` prend `topic`, `decision`, `pending` ou `fact`.
- `paul forget` décrit ce qui serait effacé et s'arrête avec le code 2 tant que `--yes` n'est pas donné.
- `paul web` ouvre une page locale (127.0.0.1) : fil des sessions, recherche, souvenirs avec correction et oubli, arbre des sujets.
- Le skill `paul:souvenirs` laisse Claude chercher dans les souvenirs dès que l'utilisateur parle du passé.

## Journaux externes, format v1

Tout fichier `*.jsonl` déposé dans `~/.claude/paul/journals/` est indexé comme une conversation. Une ligne JSON par message :

```
v        1
id       chaîne unique dans le fichier
session  identifiant de session
ts       horodatage ISO 8601 UTC
role     "user" | "claude" | "subagent" | "tool"
text     le message ; paul le masque à l'indexation
title    facultatif ; titre de la session
cwd      facultatif ; indice de dépôt pour le résumeur
```

## Venir de la version Python

À faire sur une seule machine :

1. Arrêter ce qui utilise l'ancienne base : les hooks `brain hook` de Claude Code (sessions ouvertes comprises), tout `brain web` en cours et tout processus Python de l'ancienne version.
2. Retirer les hooks `brain hook` de `~/.claude/settings.json` et le skill `~/.claude/skills/souvenirs/`.
3. Lancer `paul migrate` (options `--from` pour le chemin de l'ancienne base, `--root` pour la racine de ses projets). La commande refuse de s'exécuter si `paul.db` existe déjà. Chaque ancien projet devient un sujet, les points sont rattachés à leur sujet, les étiquettes et les accès sont abandonnés, puis l'index est reconstruit.
4. Le journal de l'assistant vocal de l'ancienne version est converti au format v1 dans `~/.claude/paul/journals/vocal.jsonl`.

L'ancienne base n'est jamais modifiée : `paul migrate` n'en lit qu'une copie.
