package main

import (
	"fmt"
	"strings"
)

// home is the landing page: ninety seconds, in plain language, ending in four doors.
//
// Written to be understood by someone who administers databases rather than someone who writes Go.
// The wiki's first page is where a reader decides whether to keep reading, and a landing page that
// opens with "capability-driven plugin contract" loses the DBA this product is actually for.
func home() string {
	return `# Fleetward

**A tool that watches the backups of every database server you are responsible for — and proves
they can actually be restored.**

## The problem it solves

Most backup tooling tells you a backup *succeeded*. What it means is that a program exited without
an error. It does not mean the file it produced can be restored, and the gap between those two
facts is where data loss lives — usually discovered on the worst possible day.

Fleetward closes that gap. It takes a backup, then restores it into a throwaway database container
of the matching engine and version, and compares what came back against a record of what the source
actually contained. Three answers come out, and they are deliberately different:

| Answer | Meaning |
|---|---|
| **verified** | Restored, and the data is all there. |
| **failed** | The backup is bad. Louder than having no backup at all, because it is more dangerous — you believed you were covered. |
| **inconclusive** | We could not check. The container would not start, the network broke. This says nothing about your backup, and it is important that it does not pretend to. |

It also reads the backups your existing scripts and cron jobs already take, so it can report on your
whole estate from the first day without you changing anything about how those backups are made.

## Where the project is

> **Pre-alpha.** The verification loop is built and proven on PostgreSQL, in both directions: a
> deliberately corrupted backup comes back *failed*, and a sandbox that never answered comes back
> *inconclusive*.
>
> **Not yet built, stated plainly:** nothing runs on a schedule — every backup is triggered by a
> person; there is no authentication, so every API route is open to anyone who can reach the port;
> and only PostgreSQL is a working engine plugin. [Project status](Project-Status) lists all of it,
> and which piece of work owns each item.

## Where to go

| | |
|---|---|
| **[Why Fleetward](Why-Fleetward)** | The problem, and who it is for |
| **[Architecture](Architecture)** | How it is built, and the one rule that shapes everything |
| **[Configuration reference](Configuration-Reference)** | Every setting, with its default |
| **[Writing an engine plugin](Writing-an-Engine-Plugin)** | Adding your own database engine |
| **[Design notes](Design-Notes)** | The decisions with the longest reach, and why |
| **[Roadmap](Roadmap)** | What comes next, and in what order |
`
}

// sidebar lists the pages worth navigating to, grouped by what a reader came for. Pages marked
// hidden are reachable and linkable but not listed: thirty-seven decision records and journal
// entries in a sidebar is a wall, and each has an index page of its own.
func sidebar(pages []page) string {
	var b strings.Builder
	b.WriteString("### [Fleetward](Home)\n\n")

	for _, a := range audiences {
		var links []string
		for _, p := range pages {
			if p.audience == a.key && !p.hidden && p.title != "" {
				links = append(links, fmt.Sprintf("- [%s](%s)", p.title, p.wiki))
			}
		}
		if len(links) == 0 {
			continue
		}
		fmt.Fprintf(&b, "**%s**\n\n%s\n\n", a.title, strings.Join(links, "\n"))
	}
	return b.String()
}

// footer says where a page came from, on every page.
//
// The wiki is generated, so editing a page here is work that is silently thrown away on the next
// merge. Saying so on every page is cheaper than explaining it afterwards.
func footer() string {
	return "---\n\n" +
		"Generated from [the repository](" + repoBase + "). " +
		"**Edits made here are overwritten on the next merge to `main`** — change the source file and " +
		"open a pull request instead.\n"
}

// sourceNote is prepended to each page, naming the file it came from.
func sourceNote(source string) string {
	return fmt.Sprintf("<!-- Generated from %s. Edits here are overwritten. -->\n\n", source)
}
