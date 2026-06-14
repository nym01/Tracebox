// Standalone module for the Phase 4 eBPF POC. Kept separate from the root
// goboxd module (github.com/nym01/goboxd) so the cilium/ebpf dependency and its
// transitive graph never enter the production build.
module ebpf-poc

go 1.25

require github.com/cilium/ebpf v0.19.0

require golang.org/x/sys v0.31.0 // indirect
