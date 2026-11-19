Implement interval merging in a file named `solution.py`.

Contract (exact name):

- `merge(intervals: list[list[int]]) -> list[list[int]]` — each input item
  is `[start, end]` with `start <= end`. Merge every group of overlapping
  or touching intervals (two intervals touch when one's end equals the
  other's start). Return the merged intervals sorted by start. An empty
  input returns an empty list.
