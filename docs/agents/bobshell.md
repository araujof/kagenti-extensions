# Bob Shell

Typing `bob` instead of `agentop exec -- bob` in every new shell, via `agentop configure bobshell`.

## Typing `bob` instead of `agentop exec -- bob` (`agentop configure bobshell`)

`agentop exec -- bob` routes one invocation. `agentop configure bobshell enable`
makes that the meaning of `bob` in every new shell, by appending a delimited
block to your rc file:

```sh
# >>> cortex agentop (bobshell) >>>
bob() {
  agentop exec -- bob "$@"
}
export CORTEX_BOBSHELL=1
# <<< cortex agentop (bobshell) <<<
```

```sh
agentop configure bobshell enable    # append the block
agentop configure bobshell disable   # remove exactly that block
agentop configure bobshell status    # is bob routed in THIS shell?
```

Both `enable` and `disable` ask before writing, and `--yes` skips the question:

```sh
agentop configure bobshell enable --yes    # no prompt
```

With no terminal to ask on — CI, a container, a Dockerfile `RUN` — they decline
rather than prompt, print `Re-run with --yes to apply`, write nothing and exit
**3**. Not 0: a decline is not a failure, but it is also not an applied change,
and 3 is what lets an unattended caller tell the two apart — the same code
`configure claude-code` and `service` return when they are declined. An
unattended caller that omits `--yes` is still a no-op; it is just no longer a
silent one. Scripted callers should pass `--yes`.

`bob` and `bobshell` are two different agents, not two spellings of one. `bob`
configures the IBM Bob **editor** — a VS Code fork, so the lever is `http.proxy`
in its `settings.json` (below). `bobshell` configures the **shell integration** —
a `bob` function in your rc file, so typing `bob` at a prompt runs through
Cortex. Configuring one does not configure the other, and neither name is an
alias of the other: `agentop configure bob --settings X` is a usage error under
`bobshell`, and vice versa.

The editor agent briefly did not exist. It was removed on the reasoning that
what needed configuring was the shell integration and "IBM Bob itself needs no
configuring", which was right about the binary and wrong about the editor. It is
back, and the two now stand side by side.

Let `enable` write it rather than pasting the block above. What `enable` appends
begins with a blank line, which the fence cannot show you: it is invisible when
rendered, and formatters strip a leading blank line inside a fence anyway. That
newline is part of the block rather than something added to your content, so the
block starts on a line of its own even after a file whose last line has no
newline of its own, and `disable` takes the separator away with the rest of it.
Paste the fence's text verbatim at the top of a file and `disable` will report a
block "edited since it was written" and decline — it matches the whole constant,
leading newline included. It declines rather than guessing, so nothing is lost,
but the block is then yours to remove by hand.

Letting `enable` write it is also why nothing else in the file is touched and the
file comes back byte-for-byte — a round-trip test asserts exactly that,
including on a file with no trailing newline, so the two halves cannot drift
apart.
Run `enable` twice and the second run is a no-op. If the block is there but
hand-edited, or there twice over, both verbs decline and say so rather than
guess which copy you meant.

A shell function, not an alias: bash does not expand aliases in
non-interactive shells unless `expand_aliases` is set, and `"$@"` forwards
arguments explicitly, so `bob "two words"` stays one argument.

It cannot recurse into itself, which is why there is no machinery to resolve
the real binary past the function. `agentop exec` runs the `bob` binary as a child
process, and that process never reads your rc file, so the `bob` inside the body
is always the one on `PATH`.

### Which file it writes

The basename of `$SHELL` picks it, and only these two:

| `$SHELL` | File |
|---|---|
| …`/zsh` | `~/.zshrc` |
| …`/bash` | `~/.bashrc` |
| anything else, or unset | nothing is written — `enable` prints the block for you to place, `disable` tells you which block to delete |

That is the whole rule. `agentop` does not work out whether your shell will be a
login or a non-login shell, or which of zsh's four startup files you meant,
because being wrong about it writes a block into a file nothing reads and
leaves you with no reason to look there.

**bash on macOS is the case to know about.** Terminal.app starts bash as a
*login* shell, which reads `~/.bash_profile` and not `~/.bashrc`. If the
function does not appear in a new window, either source `~/.bashrc` from
`~/.bash_profile` — the usual arrangement — or move the block there yourself.
`enable` always prints the path it wrote, so you can see where it went.

### Symlinks

A `~/.zshrc` symlinked into a dotfiles repo is followed, and the block lands in
the real file: writing the link itself would replace it with a regular file and
silently detach it from the repo, leaving the tracked copy stale with nothing
in `git status` to show it. Two or more links deep, or a dangling link, and
both verbs decline to write — a chain that long is somebody's deliberate
arrangement, and a write through it is more likely to surprise than to help.

What they print instead follows the verb, as it does for an unrecognised
`$SHELL`: `enable` gives you the block to paste, and `disable` names the markers
to delete between, because you already have the block — it is in your file — and
printing it at someone removing the integration reads as an instruction to put
it back.

The write is temp-file-then-rename, so a failure part way through leaves your
rc file as it was rather than half-written. An existing file keeps its own
permissions; a new one is created `0644`.

### What `status` does and does not know

It reports whether `CORTEX_BOBSHELL` is set in the environment, and it reads no
files. So it says `not enabled` in the very shell that just ran `enable`, until
you open a new terminal or source the file — recognising the block inside a
startup script would mean parsing shell, which is the thing this command
deliberately does not do.

What the variable proves is narrower than "`bob` is routed", which is why the
report separates the two. The variable is exported, so every child process inherits
it — including a **non-interactive** subshell or a script, which does not read
your startup file and therefore has no `bob` function at all. There, `bob` is the
plain binary and Cortex is not in the path of the call, while the variable still
says `1`. `status` cannot tell the two apart: the shell's function table lives in
that shell's memory and is never exported, so `agentop`, as a child process, cannot
see it. To settle it in a particular shell, ask that shell: `type bob` says
`bob is a function` when the function is live, and names a file when it is not.

Use `type`, not `which`. In bash, `which` is `/usr/bin/which` — a separate
process, which cannot see its parent shell's functions, so with the function live
it reports the `bob` binary's path and looks like a definitive "not routed". zsh's
`which` is a builtin and does report the function, so the wrong advice works in
one of the two shells this writes a file for. `type` is a shell builtin in sh,
bash, zsh and dash alike. (`mise doctor` splits `activated:` from
`shims_on_path:` for the same reason.)

Both answers exit 0: "not enabled" is a report, not a failure.

