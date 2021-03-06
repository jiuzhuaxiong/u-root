// Copyright 2012-2018 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// pox builds a portable executable as a squashfs image.
// It is intended to create files compatible with tinycore
// tcz files. One of more of the files can be programs
// but that is not required.
// This could have been a simple program but mksquashfs does not
// preserve path information.
// Yeah.
//
// Synopsis:
//     pox [-[-debug]|d] [-[-file]|f tcz-file] -[-create]|c FILE [...FILE]
//     pox [-[-debug]|d] [-[-file]|f tcz-file] -[-run|r] PROGRAM -- [...ARGS]
//
// Description:
//     pox makes portable executables in squashfs format compatible with
//     tcz format. We don't build in the execution code, rather, we set it
//     up so we can use the command itself. You can either create the TCZ image
//     or run a command within an image that was previously created.
//
// Options:
//     debug|d: verbose
//     file|f file: file name (default /tmp/pox.tcz)
//     run|r: Runs the first non-flag argument to pox.  Remaining arguments will
//            be passed to the program.  Use '--' before any flag-like arguments
//            to prevent pox from interpretting the flags.
//     create|c: create the TCZ file.
//     zip|z: Use zip and unzip instead of a loopback mounted squashfs.  Be sure
//            to use -z for both creation and running, or not at all.
//     Exactly one of -c and -r must be used on the same command.
//
// Example:
//	$ pox -c /bin/bash /bin/cat /bin/ls /etc/hosts
//	Will build a squashfs, which will be /tmp/pox.tcz
//
//	$ sudo pox -r /bin/bash
//	Will drop you into the /tmp/pox.tcz running bash
//	You can use ls and cat on /etc/hosts.
//
//	Simpler example, with arguments:
//	$ sudo pox -r /bin/ls -- -la
//	will run `ls -la` and exit.
//
//	$ sudo pox -r -- /bin/ls -la
//	Syntactically easier: the program name can come after '--'
//
// Notes:
// - When running a pox, you likely need sudo to chroot
//
// - Binaries run out of a chroot often need files you are unaware of.  For
// instance, if bash can't find terminfo files, it won't know to handle
// backspaces properly.  (They occur, but are not shown).  To fix this, pass pox
// all of the files you need.  For bash: `find /lib/terminfo -type f`.
//
// Other programs rely on help functions, such as '/bin/man'.  If your program
// has built-in help commands that trigger man pages, e.g. "git help foo",
// you'll want to include /bin/man too.  But you'll also need everything that
// man uses, such as /etc/manpath.config.  My advice: skip it.
//
// - When adding all files in a directory, the easiest thing to do is:
// `find $DIR -type f`  (Note the ticks: this is a bash command execution).
//
// - When creating a pox with an executable with shared libraries that are not
// installed on your system, such as for a project installed in your home
// directory, run pox from the installation prefix directory, such that the
// lib/ and bin/ are in pox's working directory.  Pox will strip its working
// directory from the paths of the files it builds.  Having bin/ in the root of
// the pox file helps with PATH lookups, and not having the full path from your
// machine in the pox file makes it easier to extract a pox file to /usr/local/.
//
// - Consider adding a --extract | -x option to install to the host.  One issue
// would be how to handle collisions, e.g. libc.  Your app may not like the libc
// on the system you run on.
package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	flag "github.com/spf13/pflag"
	"github.com/u-root/u-root/pkg/ldd"
	"github.com/u-root/u-root/pkg/loop"
	"github.com/u-root/u-root/pkg/mount"
)

const usage = "pox [-[-debug]|d] -[-run|r] | -[-create]|c  [-[-file]|f tcz-file] file [...file]"

var (
	debug  = flag.BoolP("debug", "d", false, "enable debug prints")
	run    = flag.BoolP("run", "r", false, "Run the first file argument")
	create = flag.BoolP("create", "c", false, "create it")
	zip    = flag.BoolP("zip", "z", false, "use zip instead of squashfs")
	file   = flag.StringP("output", "f", "/tmp/pox.tcz", "Output file")
	v      = func(string, ...interface{}) {}
)

// When chrooting, programs often want to access various system directories:
var chrootMounts = []struct {
	source string
	target string
	fstype string
	flags  uintptr
	data   string
	perm   os.FileMode // for target in the chroot
}{
	// mount --bind /sys /chroot/sys
	{"/sys", "/sys", "", mount.MS_BIND, "", 0555},
	// mount -t proc /proc /chroot/proc
	{"/proc", "/proc", "proc", 0, "", 0555},
	// mount --bind /dev /chroot/dev
	{"/dev", "/dev", "", mount.MS_BIND, "", 0755},
}

func poxCreate(names []string) error {
	if len(names) == 0 {
		return fmt.Errorf(usage)
	}
	l, err := ldd.Ldd(names)
	if err != nil {
		var stderr []byte
		if eerr, ok := err.(*exec.ExitError); ok {
			stderr = eerr.Stderr
		}
		return fmt.Errorf("Running ldd on %v: %v %s", names,
			err, stderr)
	}

	for _, dep := range l {
		v("%s", dep.FullName)
		names = append(names, dep.FullName)
	}
	// Now we need to make a template file hierarchy and put
	// the stuff we want in there.
	dir, err := ioutil.TempDir("", "pox")
	if err != nil {
		return err
	}
	if !*debug {
		defer os.RemoveAll(dir)
	}
	pwd, err := os.Getwd()
	if err != nil {
		return err
	}
	// We don't use defer() here to close files as
	// that can cause open failures with a large enough number.
	for _, f := range names {
		v("Process %v", f)
		fi, err := os.Stat(f)
		if err != nil {
			return err
		}
		in, err := os.Open(f)
		if err != nil {
			return err
		}
		f = strings.TrimPrefix(f, pwd)
		dfile := filepath.Join(dir, f)
		d := filepath.Dir(dfile)
		if err := os.MkdirAll(d, 0755); err != nil {
			in.Close()
			return err
		}
		out, err := os.OpenFile(dfile, os.O_WRONLY|os.O_CREATE,
			fi.Mode().Perm())
		if err != nil {
			in.Close()
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		out.Close()
		if err != nil {
			return err
		}

	}
	for _, m := range chrootMounts {
		if err := os.MkdirAll(filepath.Join(dir, m.target), m.perm); err != nil {
			return err
		}
	}
	err = os.Remove(*file)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var c *exec.Cmd
	if *zip {
		fileAbs, err := filepath.Abs(*file)
		if err != nil {
			return err
		}
		c = exec.Command("zip", "-r", fileAbs, ".")
		c.Dir = dir
	} else {
		c = exec.Command("mksquashfs", dir, *file, "-noappend")
	}
	o, err := c.CombinedOutput()
	v("%v", string(o))
	if err != nil {
		return fmt.Errorf("%v: %v: %v", c.Args, string(o), err)
	}
	v("Done, your pox is in %v", *file)

	return nil
}

func poxRun(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(usage)
	}
	dir, err := ioutil.TempDir("", "pox")
	if err != nil {
		return err
	}
	if !*debug {
		defer os.RemoveAll(dir)
	}

	if *zip {
		c := exec.Command("unzip", *file, "-d", dir)
		o, err := c.CombinedOutput()
		v("%v", string(o))
		if err != nil {
			return fmt.Errorf("%v: %v: %v", c.Args, string(o), err)
		}
	} else {
		lo, err := loop.New(*file, "squashfs", "")
		if err != nil {
			return err
		}
		defer lo.Free() //nolint:errcheck

		mountPoint, err := lo.Mount(dir, 0)
		if err != nil {
			return err
		}
		defer mountPoint.Unmount(0) //nolint:errcheck
	}
	for _, m := range chrootMounts {
		mp, err := mount.Mount(m.source, filepath.Join(dir, m.target),
			m.fstype, m.data, m.flags)
		if err != nil {
			return err
		}
		defer mp.Unmount(0) //nolint:errcheck
	}

	// If you pass Command a path with no slashes, it'll use PATH from the
	// parent to resolve the path to exec.  Once we chroot, whatever path we
	// picked is undoubtably wrong.  Let's help them out: if they give us a
	// program with no /, let's look in /bin/.  If they want the root of the
	// chroot, they can use "./"
	if filepath.Base(args[0]) == args[0] {
		args[0] = filepath.Join(string(os.PathSeparator), "bin", args[0])
	}
	c := exec.Command(args[0], args[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.SysProcAttr = &syscall.SysProcAttr{
		Chroot: dir,
	}
	c.Env = append(os.Environ(), "PWD=.")

	if err = c.Run(); err != nil {
		v("pox command exited with: %v", err)
	}

	return nil
}

func pox() error {
	flag.Parse()
	if *debug {
		v = log.Printf
	}
	if (*create && *run) || (!*create && !*run) {
		return fmt.Errorf(usage)
	}
	if *create {
		return poxCreate(flag.Args())
	}
	if *run {
		return poxRun(flag.Args())
	}
	return fmt.Errorf(usage)
}

func main() {
	if err := pox(); err != nil {
		log.Fatal(err)
	}
}
