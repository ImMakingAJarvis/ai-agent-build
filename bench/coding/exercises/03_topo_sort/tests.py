import solution


def valid(n, edges, order):
    """order is a permutation of 0..n-1 and satisfies every edge."""
    if sorted(order) != list(range(n)):
        return False
    pos = {v: i for i, v in enumerate(order)}
    return all(pos[u] < pos[v] for u, v in edges)


def main():
    checks = 0

    def ok(cond, msg):
        nonlocal checks
        assert cond, msg
        checks += 1

    r = solution.topo_sort(3, [(0, 1), (1, 2)])
    ok(r == [0, 1, 2], f"chain: {r}")

    r = solution.topo_sort(4, [(0, 1), (0, 2), (1, 3), (2, 3)])
    ok(r == [0, 1, 2, 3], f"diamond lexicographic: {r}")

    ok(solution.topo_sort(3, [(0, 1), (1, 2), (2, 0)]) is None, "cycle")
    ok(solution.topo_sort(2, [(1, 1)]) is None, "self-loop")

    r = solution.topo_sort(4, [(2, 3)])
    ok(r == [0, 1, 2, 3], f"disconnected: {r}")

    r = solution.topo_sort(5, [(2, 0), (3, 1), (0, 4), (1, 4)])
    ok(r == [2, 0, 3, 1, 4], f"two roots: {r}")

    r = solution.topo_sort(6, [(5, 0), (0, 1), (1, 2), (2, 3), (3, 4), (4, 5)])
    ok(r is None, "long cycle")

    n, edges = 8, [(1, 2), (1, 3), (2, 5), (3, 5), (4, 6), (4, 7), (0, 4)]
    r = solution.topo_sort(n, edges)
    ok(r is not None and valid(n, edges, r), f"valid ordering: {r}")
    ok(r == [0, 1, 2, 3, 4, 5, 6, 7], f"lexicographic: {r}")

    print(f"PASS {checks} checks")


if __name__ == "__main__":
    main()
