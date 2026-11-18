import heapq


def topo_sort(n: int, edges: list[tuple[int, int]]):
    succ = [[] for _ in range(n)]
    indeg = [0] * n
    for u, v in edges:
        succ[u].append(v)
        indeg[v] += 1
    heap = [i for i in range(n) if indeg[i] == 0]
    heapq.heapify(heap)
    order = []
    while heap:
        u = heapq.heappop(heap)
        order.append(u)
        for v in succ[u]:
            indeg[v] -= 1
            if indeg[v] == 0:
                heapq.heappush(heap, v)
    if len(order) != n:
        return None
    return order
