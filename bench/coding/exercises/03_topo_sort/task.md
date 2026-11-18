Implement a topological sort with cycle detection in a file named
`solution.py`.

Contract (exact name):

- `topo_sort(n: int, edges: list[tuple[int, int]]) -> list[int] | None`
  — nodes are `0 .. n-1`. Edge `(u, v)` means `u` must come before `v`.
  Return an ordering of all `n` nodes that satisfies every edge. When
  more than one ordering is valid, return the lexicographically smallest
  one: repeatedly place the smallest-numbered node whose predecessors
  are all already placed. Return `None` if the graph has a cycle
  (including a self-loop).
