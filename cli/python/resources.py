"""One inference per profile; average memory target and separate emergency guard."""
import asyncio
import ctypes
import fcntl
import json
import os
from pathlib import Path
import threading
import time
from contextlib import contextmanager
from collections import deque

GIB = 1024 ** 3
MEMORY_TARGET = 4 * GIB
MEMORY_STOP = 6 * GIB
ADMISSION_LIMIT = MEMORY_STOP - 512 * 1024 ** 2
AVERAGE_WINDOW = 60
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


class RequestCapacity:
    """One model call and at most three waiters; no unbounded executor queue."""
    def __init__(self, limit=4, timeout=5):
        self.limit, self.timeout = limit, timeout
        self.pending = 0
        self.lock = asyncio.Lock()
        self.started = None
        self.overdue_reported = False

    async def acquire(self):
        if self.pending >= self.limit:
            raise Busy("model request capacity reached")
        self.pending += 1
        try:
            async with asyncio.timeout(self.timeout):
                await self.lock.acquire()
                self.started = time.monotonic()
                self.overdue_reported = False
        except BaseException as exc:
            self.pending -= 1
            if isinstance(exc, TimeoutError):
                raise Busy("model queue wait exceeded 5 seconds") from None
            raise

    def healthy(self):
        # Rust polls this endpoint independently and terminates the process group.
        healthy = self.started is None or time.monotonic() - self.started < 120
        if not healthy and not self.overdue_reported:
            self.overdue_reported = True
            os.write(2, f"{int(time.time())} inference_timeout\n".encode())
        return healthy

    def submit(self, executor, function, *args):
        """Transfer slot ownership to actual work, independent of HTTP cancellation."""
        try:
            future = asyncio.get_running_loop().run_in_executor(executor, function, *args)
        except BaseException:
            self.release()
            raise
        def completed(done):
            self.release()
            # Retrieve failures even when the client has already disconnected.
            if not done.cancelled():
                done.exception()
        future.add_done_callback(completed)
        return future

    def release(self):
        self.started = None
        self.pending -= 1
        self.lock.release()


@contextmanager
def admission(wait=False):
    with open(ROOT / f"locks/{PROFILE}-inference.lock", "a+b") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | (0 if wait else fcntl.LOCK_NB))
        except BlockingIOError as exc:
            raise Busy("this model profile is processing a request") from exc
        try:
            if total_memory() >= ADMISSION_LIMIT:
                raise MemoryPressure("local memory emergency guard has insufficient headroom")
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
        samples = deque()
        while not stopped.is_set():
            try:
                total = total_memory()
                peak = max(total, peak)
                now = time.monotonic()
                samples.append((now, total))
                while samples[0][0] < now - AVERAGE_WINDOW:
                    samples.popleft()
                data = {"processes": owners, "total_bytes": total, "peak_bytes": peak,
                        "target_bytes": MEMORY_TARGET, "stop_bytes": MEMORY_STOP,
                        "average_bytes": sum(value for _, value in samples) // len(samples),
                        "average_window_seconds": AVERAGE_WINDOW,
                        "observed_seconds": round(now - samples[0][0], 2),
                        "sampled_at": time.time()}
                temp = path.with_suffix(f".tmp-{os.getpid()}")
                temp.write_text(json.dumps(data))
                temp.chmod(0o600)
                temp.replace(path)
                if total > MEMORY_STOP:
                    # The Rust supervisor withdraws and reaps this owned process group.
                    os.write(2, b"managed memory emergency guard exceeded; stopping model\n")
                    os._exit(75)
            except Exception:
                os.write(2, b"managed memory accounting failed; stopping model\n")
                os._exit(75)
            stopped.wait(0.25)

    thread = threading.Thread(target=monitor, daemon=True, name="memory-monitor")
    thread.start()
    return stopped
