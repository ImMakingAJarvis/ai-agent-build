#!/usr/bin/env python3
"""Tests for the VRAM backpressure logic in art_service.py.

These exercise wait_for_vram() with an injected free-VRAM reader, so no GPU
or torch is needed — they run on the host or in the art container:

    python3 scripts/test_art_service.py

The wait logic is what turns an intermittent "CUDA OOM" 503 into "wait for
the brain to go idle, then succeed"; these pin down its contract.
"""

import sys
import time

import art_service


def test_returns_true_immediately_when_free():
    calls = {"n": 0}

    def free():
        calls["n"] += 1
        return 20000  # well above the threshold

    t0 = time.time()
    ok = art_service.wait_for_vram(min_free_mb=12000, timeout_s=10,
                                   poll_s=0.5, free_mb_fn=free)
    assert ok is True, "should return True when VRAM is already free"
    assert calls["n"] == 1, "should not poll again after the first check"
    assert time.time() - t0 < 0.5, "should return without sleeping"


def test_returns_true_when_vram_frees_during_wait():
    # Brain is busy (low VRAM) for the first three polls, then idles out.
    seq = iter([3000, 3000, 3000, 15000])
    art_service.wait_for_vram(min_free_mb=12000, timeout_s=10,
                              poll_s=0.02, free_mb_fn=lambda: next(seq))
    # Consumed all four values: three lows then the high -> success.
    try:
        next(seq)
        raise AssertionError("expected the sequence to be fully consumed")
    except StopIteration:
        pass


def test_returns_false_on_timeout_when_never_free():
    t0 = time.time()
    ok = art_service.wait_for_vram(min_free_mb=12000, timeout_s=0.2,
                                   poll_s=0.05, free_mb_fn=lambda: 3000)
    assert ok is False, "should return False when VRAM never frees"
    # It waited roughly the timeout, not much more.
    elapsed = time.time() - t0
    assert elapsed >= 0.15, f"should wait close to the timeout ({elapsed:.3f}s)"
    assert elapsed < 1.0, f"should not overshoot the timeout badly ({elapsed:.3f}s)"


def test_returns_true_immediately_without_gpu():
    # No CUDA (reader reports None) -> nothing to wait for.
    t0 = time.time()
    ok = art_service.wait_for_vram(min_free_mb=12000, timeout_s=10,
                                   poll_s=0.5, free_mb_fn=lambda: None)
    assert ok is True, "should proceed when there is no GPU to contend over"
    assert time.time() - t0 < 0.5, "should not sleep when there is no GPU"


def test_final_check_at_deadline_wins():
    # VRAM frees exactly as the deadline arrives: the last check must see it.
    # We model this by returning low until ~timeout, then high.
    start = time.time()
    timeout = 0.2

    def free():
        return 15000 if (time.time() - start) >= timeout - 0.01 else 3000

    ok = art_service.wait_for_vram(min_free_mb=12000, timeout_s=timeout,
                                   poll_s=0.02, free_mb_fn=free)
    assert ok is True, "a slot that idles out at the deadline should still win"


def test_clear_cache_called_while_waiting():
    cleared = {"n": 0}

    def clear():
        cleared["n"] += 1

    seq = iter([3000, 3000, 15000])
    art_service.wait_for_vram(min_free_mb=12000, timeout_s=10, poll_s=0.02,
                              free_mb_fn=lambda: next(seq),
                              clear_cache_fn=clear)
    # One clear after each of the two low readings (none after the success).
    assert cleared["n"] == 2, f"expected 2 cache clears, got {cleared['n']}"


def main():
    fns = [v for k, v in sorted(globals().items()) if k.startswith("test_")]
    failed = 0
    for fn in fns:
        try:
            fn()
            print(f"ok   {fn.__name__}")
        except Exception as e:  # noqa: BLE001 - report and continue
            failed += 1
            print(f"FAIL {fn.__name__}: {e!r}")
    print(f"\n{len(fns) - failed}/{len(fns)} passed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
