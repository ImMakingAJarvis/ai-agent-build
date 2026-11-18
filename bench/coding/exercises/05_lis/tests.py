import solution


def is_subseq(seq, nums):
    it = iter(nums)
    return all(any(x == y for y in it) for x in seq)


def strictly_increasing(seq):
    return all(a < b for a, b in zip(seq, seq[1:]))


def main():
    checks = 0

    def ok(cond, msg):
        nonlocal checks
        assert cond, msg
        checks += 1

    def case(nums, want_len, msg):
        n = solution.lis_length(nums)
        ok(n == want_len, f"{msg} length: got {n}, want {want_len}")
        got = solution.lis(nums)
        ok(len(got) == n, f"{msg} reconstruction length: {got}")
        ok(is_subseq(got, nums), f"{msg} is a subsequence: {got}")
        ok(strictly_increasing(got), f"{msg} strictly increasing: {got}")

    case([0, 1, 0, 3, 2, 3], 4, "classic")
    case([1, 2, 3, 4], 4, "sorted")
    case([4, 3, 2, 1], 1, "reverse")
    case([2, 2, 2], 1, "all duplicates")
    case([7], 1, "single")
    case([], 0, "empty")
    case([10, 9, 2, 5, 3, 7, 101, 18], 4, "leetcode 300")
    case([-5, -1, -3, 0], 3, "negatives")
    case([1, 3, 2, 4], 3, "one swap")

    print(f"PASS {checks} checks")


if __name__ == "__main__":
    main()
