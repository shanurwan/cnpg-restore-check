"""Merge the JSON part files of one run, plus key=value fields, into one result file."""
import json, os, sys
parts, out, kv = sys.argv[1], sys.argv[2], sys.argv[3:]
rec = {}
for f in sorted(os.listdir(parts)):
    if f.endswith(".json"):
        with open(os.path.join(parts, f)) as fh:
            txt = fh.read().strip()
        try:
            rec[f[:-5]] = json.loads(txt) if txt else None
        except json.JSONDecodeError:
            rec[f[:-5]] = {"unparsed": txt}
for item in kv:
    k, v = item.split("=", 1)
    try:
        v = int(v)
    except ValueError:
        pass
    rec[k] = v
json.dump(rec, open(out, "w"), indent=1, sort_keys=True)
for f in os.listdir(parts):
    os.remove(os.path.join(parts, f))
os.rmdir(parts)
print(out)
