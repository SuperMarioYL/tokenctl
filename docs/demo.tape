# vhs tape — tokenctl 10-minute happy path, condensed to ~30s.
# Render in CI via .github/workflows/demo.yml, or locally:
#   go build -o /usr/local/bin/tokenctl ./cmd/tokenctl
#   vhs docs/demo.tape           # needs the `tokenctl` binary on PATH
Output assets/demo.gif

Set Shell "bash"
Set FontSize 18
Set Width 1100
Set Height 660
Set Theme "Dracula"
Set Padding 18

Type "# 1) Generate a 3-team, 6-dev org -> team -> dev budget tree"
Enter
Sleep 600ms
Type "tokenctl init --org acme -c tokenctl.yaml"
Enter
Sleep 2s

Type "# 2) Start the proxy + arbiter + metrics (backgrounded for the demo)"
Enter
Sleep 600ms
Type "tokenctl up -c tokenctl.yaml >/tmp/tokenctl.log 2>&1 &"
Enter
Sleep 2.5s

Type "# the proxy prints the base URLs to point your Coding Agent at:"
Enter
Sleep 400ms
Type "head -4 /tmp/tokenctl.log"
Enter
Sleep 2.5s

Type "# 3) Watch per-leaf token burn — tokens attribute up the tree"
Enter
Sleep 600ms
Type "tokenctl top -c tokenctl.yaml --once"
Enter
Sleep 4s

Type "kill %1"
Enter
Sleep 800ms
