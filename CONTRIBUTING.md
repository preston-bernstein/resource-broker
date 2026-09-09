# Contributing

Contributions are welcome. This is a small project maintained by one person in evenings, so this file explains what gets merged quickly, what gets closed, and why.

## Good first changes

The detection rules only cover what the maintainer runs: Steam, Lutris, Heroic, Wine, Plex, and Tdarr. Adding another game launcher or another transcoder is small, self-contained, and genuinely useful. Start there.

See [`internal/detect/detect.go`](internal/detect/detect.go) for how a rule is written, and add a case to `detect_test.go` alongside it.

## Before you open a pull request

- **Say what problem it solves.** One or two sentences. A change with no stated problem is hard to review and usually gets questions instead of a merge.
- **Add a test.** The detection rules have tests alongside them in `detect_test.go`. New behaviour needs one that fails without your change.
- **Run `make test` locally.** CI runs the same thing, and a red build is the most common reason a pull request sits.
- **Keep it to one change.** Two unrelated fixes in one pull request take longer to review than two pull requests.
- **Check the non-goals** in the README first. A change that fights the project's scope will be closed, and neither of us gets that time back.

Design decisions live in [`docs/adr/`](docs/adr/), one record per decision. If your change reverses one of them, say so and say why. That is a legitimate thing to propose; it just needs to be deliberate rather than accidental.

## On AI-assisted contributions

AI-assisted contributions are welcome. Unverified ones are not.

This follows the same line that [curl](https://github.com/curl/curl/blob/master/docs/CONTRIBUTE.md) and [Ghostty](https://github.com/ghostty-org/ghostty/blob/main/AI_POLICY.md) settled on, for the same reason. The problem was never the tool.

If you used a model to help write a change:

- **Say so** in the pull request, and roughly how much of it.
- **Understand every line you submit.** You will be asked about it, and "the model wrote that part" ends the review.
- **Run it.** Not just the tests. Actually start the binary and watch it do the thing.

Generated code that the submitter cannot explain gets closed without a detailed review. That is not a judgement about AI. It is that reviewing code nobody understands costs more than writing it did, and that cost lands on one person here.

Curl's version of this rule is the clearest one written down, and it applies here too: if a reviewer can tell the contribution came from a model, there is more work to do.

## Reporting a bug

Include the distro and kernel, the GPU and driver, the Ollama version, what was running when the problem happened, and the output of `curl localhost:11437/status`. Detection reads `/proc`, so a hardened or containerised setup can change behaviour, and that context saves a round trip.
