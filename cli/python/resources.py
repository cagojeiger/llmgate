"""One inference per managed home; measured Mac footprint and bounded MLX cache."""
import ctypes
import fcntl
import json
import os
from pathlib import Path
import threading
import time
from contextlib import contextmanager

GIB = 1024 ** 3
MEMORY_BUDGET = 4 * GIB
ADMISSION_LIMIT = MEMORY_BUDGET - 512 * 1024 ** 2
ROOT = Path(os.environ["LLMGATE_MANAGED_ROOT"])
PROFILE = os.environ["LLMGATE_PROFILE"]


class Usage(ctypes.Structure):
    # Darwin sys/resource.h rusage_info_v2. UUID followed by 18 uint64 fields.
    _fields_ = [("uuid", ctypes.c_ubyte * 16), ("values", ctypes.c_uint64 * 18)]


LIB = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
LIB.proc_pid_rusage.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_void_p]
LIB.proc_pid_rusage.restype = ctypes.c_int


def usage(pid):
    data = Usage()
    if LIB.proc_pid_rusage(pid, 2, ctypes.byref(data)) != 0:
        return None
    return {"pid": pid, "started": data.values[8], "bytes": data.values[7]}


def total_memory():
    processes = {}
    for name in ("embedding", "stt"):
        try:
            data = json.loads((ROOT / f"control/{name}-memory.json").read_text())
            for saved in data["processes"]:
                current = usage(saved["pid"])
                if current and current["started"] == saved["started"]:
                    processes[current["pid"]] = current["bytes"]
        except (OSError, ValueError, KeyError):
            continue
    # Count this process even before its first atomic receipt is written.
    for pid in (os.getpid(), os.getppid()):
        current = usage(pid)
        if current:
            processes[pid] = current["bytes"]
    return sum(processes.values())


class Busy(Exception):
    pass


class MemoryPressure(Exception):
    pass


@contextmanager
def admission(wait=False):
    with open(ROOT / "locks/inference.lock", "a+b") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | (0 if wait else fcntl.LOCK_NB))
        except BlockingIOError as exc:
            raise Busy("another local model is processing a request") from exc
        try:
            if total_memory() >= ADMISSION_LIMIT:
                raise MemoryPressure("local memory budget has insufficient headroom")
            yield
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)


def configure_mlx():
    import mlx.core as mx
    # These are MLX allocator guidelines, not an OS process memory ceiling.
    mx.set_memory_limit(1536 * 1024 ** 2)
    mx.set_cache_limit(64 * 1024 ** 2)


def start_monitor():
    stopped = threading.Event()
    path = ROOT / f"control/{PROFILE}-memory.json"
    owners = [usage(pid) for pid in (os.getpid(), os.getppid())]
    if any(owner is None for owner in owners):
        raise RuntimeError("cannot account for managed processes")

    def monitor():
        peak = 0
        while not stopped.is_set():
            try:
                total = total_memory()
                peak = max(total, peak)
                data = {"processes": owners, "total_bytes": total, "peak_bytes": peak,
                        "budget_bytes": MEMORY_BUDGET, "sampled_at": time.time()}
                temp = path.with_suffix(f".tmp-{os.getpid()}")
                temp.write_text(json.dumps(data))
                temp.chmod(0o600)
                temp.replace(path)
                if total > MEMORY_BUDGET:
                    # The Rust supervisor withdraws and reaps this owned process group.
                    os.write(2, b"managed memory budget exceeded; stopping model\n")
                    os._exit(75)
            except Exception:
                os.write(2, b"managed memory accounting failed; stopping model\n")
                os._exit(75)
            stopped.wait(0.25)

    thread = threading.Thread(target=monitor, daemon=True, name="memory-budget")
    thread.start()
    return stopped
