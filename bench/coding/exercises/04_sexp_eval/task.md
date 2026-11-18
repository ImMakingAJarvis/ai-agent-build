Implement an s-expression parser and an arithmetic evaluator in a file
named `solution.py`.

Contract (exact names):

- `parse(text: str)` — parse exactly one s-expression from `text` and
  return it: a number (int or float, unary minus allowed, e.g. `-3`,
  `2.5`), one of the operator symbols `+`, `-`, `*`, `/` (returned as a
  string), or a nested list of such values. Whitespace separates tokens.
  Raise `ValueError` for anything malformed: unbalanced parentheses,
  trailing tokens after one complete expression, or atoms that are
  neither numbers nor operator symbols.

- `eval_arith(text: str)` — parse and evaluate an arithmetic
  s-expression. Supported forms:
  - `(+ a b ...)`, `(- a b ...)`, `(* a b ...)`, `(/ a b)` — `+`, `*`
    and `-` take two or more arguments (left-associative); `/` takes
    exactly two.
  - `(- x)` — unary minus, one argument.
  - numbers — int or float literals.
  `/` is true division and always returns a float. Nested forms are
  allowed, e.g. `(+ 1 (* 2 (- 4 1)))`. Raise `ValueError` for unknown
  operators, wrong arity, or non-numeric operands.
