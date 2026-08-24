# TODO

- Always ask user what to do next.
- Do not assume items are listed in any particular order.
- Refuse to implement items without a well-formed description.
- Once done, mark the item as completed, and move it to docs/DONE.md

## Runkube

### Log level per component

Everything is silent (LevelError) by default. But we should be able to override
the log level per-component. Propagate the log level to nodes as well.

### Lazy initialization

Everything is re(initialized) at startup with `c.Init(). We want to be lazy.
For example, config files, kubeconfigs and certificates should be re-written
on demand, if they are out of date.

## Sandbox

### File IO improvements

Every file is a new memfd today, and they are not writable? In either case,
sometimes we want to emulate files as TRO, RW or RDWR. Lets support that.

Also, instead of returning new unique memfd for every open, lets create a file
within the rootfs on first use, and return an open descriptor to that instead,
following the requested file open mode bits.

For files like in proc, where we cannot create the emulated files in the
original path, create a directory called `/.emulated` instead.

### Disk usage

Model filesystem disk space and inode space usage for the configured mount points.
Emulate a reasonable default value for the interesting mount points.

## CRI model

### Container process lifecycle

While we model container lifecycle states (Start/Stop/Create/Delete, etc), we
should also model container starts and stops as well. Real processes sometimes
exit successfully, sometimes not. We should model that behavior.

Container processes should sometimes stop after a realistic number of ticks
(to make kubelet to restart them for us).

### Disk usage

Container images, containers and logs should consume "space". This usage should
feed into the system model, and ultimately through statfs calls.
