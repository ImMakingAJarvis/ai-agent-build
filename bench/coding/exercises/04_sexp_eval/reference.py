import re

_OPS = {"+", "-", "*", "/"}

_TOKEN = re.compile(r"\(|\)|[^\s()]+")


def _num(tok):
    try:
        return int(tok)
    except ValueError:
        return float(tok)


def parse(text: str):
    toks = _TOKEN.findall(text)
    if not toks:
        raise ValueError("empty input")
    pos = 0

    def take_one():
        nonlocal pos
        if pos >= len(toks):
            raise ValueError("unexpected end of input")
        tok = toks[pos]
        pos += 1
        if tok == "(":
            items = []
            while True:
                if pos >= len(toks):
                    raise ValueError("unbalanced parentheses")
                if toks[pos] == ")":
                    pos += 1
                    if not items:
                        raise ValueError("empty list")
                    return items
                items.append(take_one())
        elif tok == ")":
            raise ValueError("unexpected ')'")
        elif tok in _OPS:
            return tok
        else:
            try:
                return _num(tok)
            except ValueError:
                raise ValueError(f"bad atom {tok!r}")

    val = take_one()
    if pos != len(toks):
        raise ValueError("trailing tokens")
    return val


def _eval(node):
    if isinstance(node, (int, float)):
        return node
    if not isinstance(node, list):
        raise ValueError("not a list")
    op, args = node[0], node[1:]
    if not isinstance(op, str) or op not in _OPS:
        raise ValueError(f"unknown operator {op!r}")
    if op == "/":
        if len(args) != 2:
            raise ValueError("/ takes exactly two arguments")
        return _eval(args[0]) / _eval(args[1])
    if op == "-":
        if len(args) == 1:
            return -_eval(args[0])
    if len(args) < 2:
        raise ValueError(f"{op} needs at least two arguments")
    vals = [_eval(a) for a in args]
    if op == "+":
        return sum(vals)
    if op == "-":
        return vals[0] - sum(vals[1:])
    if op == "*":
        out = 1
        for v in vals:
            out *= v
        return out
    raise ValueError("unreachable")


def eval_arith(text: str):
    return _eval(parse(text))
