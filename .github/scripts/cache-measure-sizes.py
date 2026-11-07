import json
import os
import subprocess
import time

started = time.monotonic()
paths = json.loads(subprocess.check_output(['go', 'env', '-json', 'GOCACHE', 'GOMODCACHE'], text=True))
for name, path in paths.items():
    size = files = 0
    for root, _, names in os.walk(path):
        for filename in names:
            size += os.stat(os.path.join(root, filename), follow_symlinks=False).st_size
            files += 1
    print(json.dumps({'cache': name, 'bytes': size, 'files': files}))
print(json.dumps({'measurement_seconds': time.monotonic() - started}))
