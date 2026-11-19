Implement an LRU (least-recently-used) cache in a file named `solution.py`.

Contract (exact names):

- `LRUCache(capacity: int)` — constructor. `capacity >= 1`.
- `get(key: int) -> int` — return the value stored for `key`, or `-1` if
  absent. A successful `get` marks the key as most-recently-used.
- `put(key: int, value: int) -> None` — insert or update the key's value
  and mark it most-recently-used. If the cache exceeds `capacity`, evict
  the least-recently-used key.

Both operations must be amortized O(1). Keys are ints, values are ints.
