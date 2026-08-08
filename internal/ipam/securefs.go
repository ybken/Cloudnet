package ipam

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const directoryOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW

// openNetworkStateDir creates and opens the state hierarchy one component at
// a time. Every lookup is anchored to an already verified directory fd, so a
// symlink cannot redirect state operations outside the configured root.
func openNetworkStateDir(root, networkName string) (*os.File, error) {
	rootDir, err := openOrCreateDirectoryPath(root)
	if err != nil {
		return nil, fmt.Errorf("open state root %s: %w", root, err)
	}
	defer rootDir.Close()

	networksDir, err := openOrCreateDirectoryAt(rootDir, "networks", 0o700)
	if err != nil {
		return nil, fmt.Errorf("open networks directory under %s: %w", root, err)
	}
	defer networksDir.Close()
	if err := networksDir.Chmod(0o700); err != nil {
		return nil, fmt.Errorf("chmod networks directory under %s: %w", root, err)
	}

	networkDir, err := openOrCreateDirectoryAt(networksDir, networkName, 0o700)
	if err != nil {
		return nil, fmt.Errorf("open network state directory %s: %w", filepath.Join(root, "networks", networkName), err)
	}
	if err := networkDir.Chmod(0o700); err != nil {
		networkDir.Close()
		return nil, fmt.Errorf("chmod network state directory %s: %w", filepath.Join(root, "networks", networkName), err)
	}
	return networkDir, nil
}

func openOrCreateDirectoryPath(path string) (*os.File, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return nil, fmt.Errorf("directory path %q is not absolute", path)
	}
	if clean == string(filepath.Separator) {
		return nil, fmt.Errorf("refuse filesystem root as state root")
	}

	fd, err := unix.Open(string(filepath.Separator), directoryOpenFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	currentDir := os.NewFile(uintptr(fd), string(filepath.Separator))
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		next, openErr := openOrCreateDirectoryAt(currentDir, component, 0o700)
		_ = currentDir.Close()
		if openErr != nil {
			return nil, fmt.Errorf("open directory component %s: %w", filepath.Join(current, component), openErr)
		}
		current = filepath.Join(current, component)
		currentDir = next
	}
	return currentDir, nil
}

func openOrCreateDirectoryAt(parent *os.File, name string, mode uint32) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, directoryOpenFlags, 0)
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(int(parent.Fd()), name, mode); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("create directory %q: %w", name, err)
		}
		fd, err = unix.Openat(int(parent.Fd()), name, directoryOpenFlags, 0)
	}
	if err != nil {
		return nil, classifyUnsafeEntry(parent, name, "directory", err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

func openRegularFileAt(parent *os.File, name, displayPath string, flags int, mode uint32) (*os.File, error) {
	fd, err := unix.Openat(
		int(parent.Fd()),
		name,
		flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		mode,
	)
	if err != nil {
		return nil, classifyUnsafeEntry(parent, name, "regular file", err)
	}
	file := os.NewFile(uintptr(fd), displayPath)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect %s: %w", displayPath, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		file.Close()
		return nil, fmt.Errorf("%s is not a regular file", displayPath)
	}
	if stat.Nlink != 1 {
		file.Close()
		return nil, fmt.Errorf("%s has %d hard links, want exactly one", displayPath, stat.Nlink)
	}
	return file, nil
}

func requireRegularOrAbsentAt(parent *os.File, name, displayPath string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", displayPath, err)
	}
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		return fmt.Errorf("%s is a symbolic link", displayPath)
	case unix.S_IFREG:
		return nil
	default:
		return fmt.Errorf("%s is not a regular file", displayPath)
	}
}

func classifyUnsafeEntry(parent *os.File, name, expected string, cause error) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
			return fmt.Errorf("%q is a symbolic link", name)
		}
		return fmt.Errorf("%q is not a %s: %w", name, expected, cause)
	}
	return fmt.Errorf("open %q as %s: %w", name, expected, cause)
}
