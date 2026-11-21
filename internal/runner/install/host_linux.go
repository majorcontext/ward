package install

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

var fsNames = map[int64]string{0x6969: "nfs", 0xFF534D42: "cifs", 0x517B: "smb", 0x65735546: "fuse", 0x01021997: "9p", 0xC36400: "ceph", 0x0BD00BD0: "lustre",
	0xEF53: "ext4", 0x58465342: "xfs", 0x9123683E: "btrfs", 0x01021994: "tmpfs", 0x794C7630: "overlayfs", 0x2FC12FC1: "zfs"}

// SystemHost reads the real machine.
func SystemHost() Host {
	return Host{
		LookPath: exec.LookPath,
		KVM:      kvmUsable,
		FSType: func(path string) (string, error) {
			var st syscall.Statfs_t
			if err := syscall.Statfs(path, &st); err != nil {
				return "", err
			}
			if n, ok := fsNames[int64(st.Type)]; ok {
				return n, nil
			}
			return "0x" + strconv.FormatInt(int64(st.Type), 16), nil
		},
		LookupUser: func(name string) (int, int, error) {
			u, err := user.Lookup(name)
			if err != nil {
				return 0, 0, err
			}
			uid, _ := strconv.Atoi(u.Uid)
			gid, _ := strconv.Atoi(u.Gid)
			return uid, gid, nil
		},
		HasSystemd: func() bool { _, err := os.Stat("/run/systemd/system"); return err == nil },
	}
}

// kvmUsable reports whether the named user, through its owner, group or other bits, may read and write /dev/kvm.
func kvmUsable(name string) error {
	st, err := os.Stat("/dev/kvm")
	if err != nil {
		return err
	}
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read the owner of /dev/kvm")
	}
	perm := uint32(st.Mode().Perm())
	allowed := perm&0o006 == 0o006 || u.Uid == "0"
	if u.Uid == strconv.Itoa(int(sys.Uid)) {
		allowed = allowed || perm&0o600 == 0o600
	}
	groups, _ := u.GroupIds()
	for _, g := range append(groups, u.Gid) {
		if g == strconv.Itoa(int(sys.Gid)) {
			allowed = allowed || perm&0o060 == 0o060
		}
	}
	if !allowed {
		return fmt.Errorf("user %q cannot read and write /dev/kvm", name)
	}
	return nil
}
