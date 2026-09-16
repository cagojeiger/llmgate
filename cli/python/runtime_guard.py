"""Hold inherited ownership/cache leases; stop when the supervisor pipe closes."""
import os
import runpy
import sys
import threading

# Inherited file descriptions retain flock even after a supervisor SIGKILL.
leases = [int(os.environ.pop(name)) for name in ("LLMGATE_OWNER_FD", "LLMGATE_CACHE_FD")]
for fd in leases:
    os.fstat(fd)
    os.set_inheritable(fd, False)


def watch_supervisor():
    # No input is sent. EOF means the owning supervisor has exited.
    while os.read(0, 1):
        pass
    os._exit(75)


threading.Thread(target=watch_supervisor, daemon=True).start()
sys.argv = sys.argv[1:]
runpy.run_path(sys.argv[0], run_name="__main__")
