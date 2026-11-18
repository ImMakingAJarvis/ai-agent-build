import solution


def main():
    checks = 0

    def ok(got, want, msg):
        nonlocal checks
        assert got == want, f"{msg}: got {got}, want {want}"
        checks += 1

    ok(solution.merge([[1, 3], [2, 6], [8, 10], [15, 18]]),
       [[1, 6], [8, 10], [15, 18]], "basic")
    ok(solution.merge([[1, 4], [4, 5]]), [[1, 5]], "touching endpoints merge")
    ok(solution.merge([[1, 10], [2, 3]]), [[1, 10]], "nested interval")
    ok(solution.merge([]), [], "empty input")
    ok(solution.merge([[7, 9]]), [[7, 9]], "single interval")
    ok(solution.merge([[8, 10], [1, 3], [2, 6]]),
       [[1, 6], [8, 10]], "unsorted input")
    ok(solution.merge([[5, 5], [1, 2], [5, 6]]),
       [[1, 2], [5, 6]], "zero-length interval merges with overlap")
    ok(solution.merge([[1, 2], [3, 4]]), [[1, 2], [3, 4]], "gap stays a gap")

    print(f"PASS {checks} checks")


if __name__ == "__main__":
    main()
