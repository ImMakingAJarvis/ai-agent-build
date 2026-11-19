import solution


def raises_value_error(fn, msg):
    try:
        fn()
    except ValueError:
        return True
    except Exception:
        return False
    raise AssertionError(f"{msg}: expected ValueError, none raised")


def main():
    checks = 0

    def ok(cond, msg):
        nonlocal checks
        assert cond, msg
        checks += 1

    ok(solution.parse("(+ 1 2)") == ["+", 1, 2], "parse list")
    ok(solution.parse("42") == 42, "parse int")
    ok(solution.parse("(- 3)") == ["-", 3], "parse unary minus form")
    ok(solution.parse("-3") == -3, "parse negative number")
    ok(solution.parse("(+ (- 1 2) 3)") == ["+", ["-", 1, 2], 3], "parse nested")
    ok(solution.parse("(1 2)") == [1, 2], "parse list of numbers")
    ok(solution.parse("2.5") == 2.5, "parse float")
    ok(raises_value_error(lambda: solution.parse("(1 2"), "unbalanced") is True, "unbalanced parens")
    ok(raises_value_error(lambda: solution.parse("1 2"), "trailing") is True, "trailing tokens")
    ok(raises_value_error(lambda: solution.parse("(x y)"), "bad atom") is True, "non-numeric atom")

    ok(solution.eval_arith("(+ 1 2)") == 3, "add")
    ok(solution.eval_arith("(+ 1 2 3)") == 6, "n-ary add")
    ok(solution.eval_arith("(- 5 2)") == 3, "sub")
    ok(solution.eval_arith("(- 5 2 1)") == 2, "n-ary sub")
    ok(solution.eval_arith("(* 3 4)") == 12, "mul")
    ok(solution.eval_arith("(/ 10 2)") == 5.0, "div float")
    ok(solution.eval_arith("(/ 10 4)") == 2.5, "div non-integral")
    ok(solution.eval_arith("(- 3)") == -3, "unary minus")
    ok(solution.eval_arith("(- (- 1 10))") == 9, "nested unary")
    ok(solution.eval_arith("(* 2 (+ 3 4))") == 14, "nested")
    ok(solution.eval_arith("(+ 1 (* 2 (- 4 1)))") == 7, "classic nested")
    ok(solution.eval_arith("(+ 1.5 2.25)") == 3.75, "floats")
    ok(42 in (solution.eval_arith("42"),), "bare number")
    ok(raises_value_error(lambda: solution.eval_arith("(x 1 2)"), "unknown op") is True, "unknown op")
    ok(raises_value_error(lambda: solution.eval_arith("(/ 1 2 3)"), "div arity") is True, "div arity")

    print(f"PASS {checks} checks")


if __name__ == "__main__":
    main()
