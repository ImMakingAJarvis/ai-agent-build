import solution


def main():
    checks = 0

    def ok(cond, msg):
        nonlocal checks
        assert cond, msg
        checks += 1

    c = solution.LRUCache(2)
    c.put(1, 1)
    c.put(2, 2)
    ok(c.get(1) == 1, "get existing")
    c.put(3, 3)  # evicts 2
    ok(c.get(2) == -1, "evicted")
    c.put(4, 4)  # evicts 1
    ok(c.get(1) == -1, "evicted again")
    ok(c.get(3) == 3 and c.get(4) == 4, "remaining keys intact")
    ok(c.get(99) == -1, "absent key")

    c2 = solution.LRUCache(2)
    c2.put(1, 1)
    c2.put(2, 2)
    c2.put(1, 10)  # update refreshes recency
    c2.put(3, 3)  # evicts 2
    ok(c2.get(1) == 10 and c2.get(2) == -1, "update refreshes recency")

    c3 = solution.LRUCache(2)
    c3.put(1, 1)
    c3.put(2, 2)
    c3.get(1)  # get refreshes recency
    c3.put(3, 3)  # evicts 2
    ok(c3.get(2) == -1 and c3.get(1) == 1, "get refreshes recency")

    c4 = solution.LRUCache(1)
    c4.put(1, 1)
    c4.put(2, 2)
    ok(c4.get(1) == -1 and c4.get(2) == 2, "capacity 1")

    print(f"PASS {checks} checks")


if __name__ == "__main__":
    main()
