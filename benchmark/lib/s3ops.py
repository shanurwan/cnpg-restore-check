"""Bucket operations used by the benchmark: list, inspect and damage barman-cloud backups.

Every command prints one JSON object, so the shell scripts can record exactly what was done.
"""
import hashlib, io, json, os, sys, tarfile
import boto3

BUCKET = "backups"
s3 = boto3.client("s3", endpoint_url=os.environ["S3_ENDPOINT"])


def out(**kw):
    print(json.dumps(kw))


def keys(prefix):
    res = []
    for page in s3.get_paginator("list_objects_v2").paginate(Bucket=BUCKET, Prefix=prefix):
        res += [(o["Key"], o["Size"]) for o in page.get("Contents", [])]
    return res


def exists(key):
    try:
        s3.head_object(Bucket=BUCKET, Key=key)
        return True
    except s3.exceptions.ClientError:
        return False


def base_prefix(prefix, server):
    return f"{prefix}{server}/base/"


def latest_backup(prefix, server):
    ids = sorted({k.split("/")[len(base_prefix(prefix, server).split("/")) - 1]
                  for k, _ in keys(base_prefix(prefix, server))})
    return ids[-1]


def backup_info(prefix, server, bid):
    body = s3.get_object(Bucket=BUCKET, Key=f"{base_prefix(prefix, server)}{bid}/backup.info")["Body"].read().decode()
    return dict(line.split("=", 1) for line in body.splitlines() if "=" in line)


def wal_key(prefix, server, segment):
    """The object holding the WAL segment itself (e.g. 000000010000000000000003.gz),
    not the backup label (000000010000000000000003.00000028.backup.gz) that shares its prefix."""
    for k, _ in keys(f"{prefix}{server}/wals/{segment[:16]}/"):
        name = k.rsplit("/", 1)[-1]
        if name.split(".", 1)[0] == segment and ".backup" not in name and ".partial" not in name:
            return k
    return None


cmd, args = sys.argv[1], sys.argv[2:]

if cmd == "mkbucket":
    try:
        s3.create_bucket(Bucket=BUCKET)
    except s3.exceptions.ClientError as e:
        if "BucketAlready" not in str(e):
            raise
    out(bucket=BUCKET, ok=True)

elif cmd == "summary":            # prefix server
    prefix, server = args
    ks = keys(f"{prefix}{server}/")
    out(objects=len(ks), bytes=sum(s for _, s in ks),
        base_bytes=sum(s for k, s in ks if "/base/" in k),
        wal_bytes=sum(s for k, s in ks if "/wals/" in k))

elif cmd == "delete-wal":         # prefix server segment
    prefix, server, seg = args
    k = wal_key(prefix, server, seg)
    if k is None:
        out(ok=False, error="segment not found", segment=seg)
        sys.exit(1)
    s3.delete_object(Bucket=BUCKET, Key=k)
    out(ok=not exists(k), deleted=k)

elif cmd == "delete-backup-begin-wal":   # prefix server : delete the first WAL segment of the latest base backup
    prefix, server = args
    bid = latest_backup(prefix, server)
    info = backup_info(prefix, server, bid)
    seg = info["begin_wal"]
    k = wal_key(prefix, server, seg)
    if k is None:
        out(ok=False, error="begin_wal not archived", backup=bid, begin_wal=seg)
        sys.exit(1)
    s3.delete_object(Bucket=BUCKET, Key=k)
    out(ok=not exists(k), backup=bid, begin_wal=seg, end_wal=info.get("end_wal"), deleted=k)

elif cmd == "delete-base-data":   # prefix server : delete the data archive(s) of the latest base backup
    prefix, server = args
    bid = latest_backup(prefix, server)
    deleted = []
    for k, _ in keys(f"{base_prefix(prefix, server)}{bid}/"):
        if not k.endswith("backup.info"):
            s3.delete_object(Bucket=BUCKET, Key=k)
            deleted.append(k)
    left = [k for k, _ in keys(f"{base_prefix(prefix, server)}{bid}/")]
    out(ok=bool(deleted) and left == [f"{base_prefix(prefix, server)}{bid}/backup.info"], backup=bid, deleted=deleted)

elif cmd == "corrupt-heap":       # prefix server relpath page : damage line pointers of one heap page
    prefix, server, relpath, page = args[0], args[1], args[2], int(args[3])
    bid = latest_backup(prefix, server)
    data_keys = [k for k, _ in keys(f"{base_prefix(prefix, server)}{bid}/") if k.endswith("data.tar")]
    if not data_keys:
        out(ok=False, error="no uncompressed data.tar in backup", backup=bid)
        sys.exit(1)
    key = data_keys[0]
    raw = s3.get_object(Bucket=BUCKET, Key=key)["Body"].read()
    src = tarfile.open(fileobj=io.BytesIO(raw), mode="r:")
    dst_buf = io.BytesIO()
    dst = tarfile.open(fileobj=dst_buf, mode="w:", format=src.format)
    hit = None
    for m in src.getmembers():
        f = src.extractfile(m) if m.isfile() else None
        data = f.read() if f else None
        if m.isfile() and m.name.lstrip("./") == relpath:
            b = bytearray(data)
            off = page * 8192 + 24              # first line pointers after the 24-byte page header
            before = hashlib.md5(b).hexdigest()
            b[off:off + 64] = b"\xff" * 64       # 16 line pointers now point far past the page
            data = bytes(b)
            hit = dict(member=m.name, size=m.size, page=page, offset=off, md5_before=before,
                       md5_after=hashlib.md5(data).hexdigest())
        dst.addfile(m, io.BytesIO(data) if data is not None else None)
    dst.close()
    if hit is None:
        out(ok=False, error="relation file not found in data.tar", relpath=relpath, backup=bid)
        sys.exit(1)
    s3.put_object(Bucket=BUCKET, Key=key, Body=dst_buf.getvalue())
    out(ok=True, backup=bid, key=key, **hit)

elif cmd == "prefixes":           # top-level run prefixes and their size
    sizes = {}
    for k, sz in keys(""):
        sizes[k.split("/", 1)[0]] = sizes.get(k.split("/", 1)[0], 0) + sz
    out(prefixes=sizes, total_bytes=sum(sizes.values()))

elif cmd == "delete-prefix":      # prefix : remove a finished run's backups to free space
    prefix = args[0]
    assert prefix.endswith("/") and len(prefix) > 1, "refusing an empty or bare prefix"
    ks = [k for k, _ in keys(prefix)]
    for i in range(0, len(ks), 1000):
        s3.delete_objects(Bucket=BUCKET, Delete={"Objects": [{"Key": k} for k in ks[i:i + 1000]]})
    out(ok=not keys(prefix), deleted_objects=len(ks), prefix=prefix)

else:
    out(ok=False, error=f"unknown command {cmd}")
    sys.exit(2)
