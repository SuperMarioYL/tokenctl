# VHS tape for tokenctl demo — render with: vhs assets/demo.tape
# Produces assets/demo.gif. (asciinema alt: asciinema rec assets/demo.cast)
Output assets/demo.gif
Set FontSize 18
Set Width 1200
Set Height 700
Set Theme "Dracula"

Type "tokenctl serve --config configs/tokenctl.example.yaml"
Enter
Sleep 2s

Type "# point your agents at the proxy"
Enter
Type "export ANTHROPIC_BASE_URL=http://localhost:8787"
Enter
Sleep 1s

Type "tokenctl top   # live per-team token burn"
Enter
Sleep 3s

Type "# team 'growth' crosses 80% -> soft-throttle (delay queue)"
Enter
Sleep 2s

Type "# team 'growth' crosses 100% -> 429 + X-TokenCtl-Reason"
Enter
Sleep 2s

Type "# high-weight 'platform' request preempts a low-weight sibling"
Enter
Sleep 3s
