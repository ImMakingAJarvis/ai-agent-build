import bisect


def lis_length(nums: list[int]) -> int:
    tails = []
    for x in nums:
        i = bisect.bisect_left(tails, x)
        if i == len(tails):
            tails.append(x)
        else:
            tails[i] = x
    return len(tails)


def lis(nums: list[int]) -> list[int]:
    if not nums:
        return []
    tails = []
    parent = [-1] * len(nums)
    tail_idx = []
    for i, x in enumerate(nums):
        j = bisect.bisect_left(tails, x)
        if j == len(tails):
            tails.append(x)
            tail_idx.append(i)
        else:
            tails[j] = x
            tail_idx[j] = i
        parent[i] = tail_idx[j - 1] if j > 0 else -1
    out = []
    i = tail_idx[-1]
    while i != -1:
        out.append(nums[i])
        i = parent[i]
    return list(reversed(out))
