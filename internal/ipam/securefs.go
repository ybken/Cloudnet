package ipam

// 本文件实现了安全的文件系统操作——cloudnet 的 "secure filesystem" 层。
//
// 为什么需要这个文件？
//   cloudnet 的状态数据存储在 /var/lib/cloudnet/networks/cloudnet-v1/ 下。
//   恶意进程可能尝试通过符号链接攻击（symlink attack）来欺骗 cloudnet
//   读取/写入任意文件。例如：
//     - 把 state.json 做成指向 /etc/shadow 的符号链接
//     - 把 .lock 做成指向 /etc/passwd 的符号链接
//     - 把 networks/cloudnet-v1 目录做成指向 /tmp/evil 的符号链接
//
//   防御策略（纵深防御）：
//     1. 逐组件使用 openat + O_NOFOLLOW：每一步都基于已验证的父目录 fd 打开
//     2. 拒绝符号链接：O_NOFOLLOW 确保不会跟随 symlink
//     3. 拒绝非普通文件：锁定文件（.lock）和状态文件（state.json）必须是普通文件
//     4. 拒绝多硬链接：Nlink 必须为 1（不能有硬链接指向同一 inode 的别名）
//     5. 临时文件原子替换：先写临时文件 → fsync → renameat → 目录 fsync
//
//   这些防御手段保证了即使 /var/lib/cloudnet 下存在恶意的符号链接或硬链接，
//   cloudnet 也不会被欺骗去读写目标之外的文件。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// directoryOpenFlags 是所有目录打开操作的公共标志：
//   - O_RDONLY：只读打开（目录不需要写权限）
//   - O_DIRECTORY：要求打开的目标必须是目录
//   - O_CLOEXEC：exec 时关闭 fd（防止子进程继承 fd）
//   - O_NOFOLLOW：不跟随符号链接（核心安全措施）
const directoryOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW

// openNetworkStateDir 逐组件创建并打开状态目录层级。
//
// 为什么不能直接 os.MkdirAll？
//
//	os.MkdirAll 没有符号链接保护。如果 /var/lib/cloudnet/networks 是一个
//	指向 /tmp/attacker 的符号链接，MkdirAll 会在 /tmp/attacker 下创建目录。
//
// 本函数的做法：
//
//	从根目录 / 开始，用 openat 逐层打开每个路径组件（每步都带 O_NOFOLLOW），
//	如果组件不存在就用 Mkdirat 创建，最终返回 cloudnet-v1 目录的 *os.File。
//
// 路径示例：root="/var/lib/cloudnet", networkName="cloudnet-v1"
//
//	最终打开的目录：/var/lib/cloudnet/networks/cloudnet-v1
func openNetworkStateDir(root, networkName string) (*os.File, error) {
	// 第一步：打开/创建 root 目录（如 /var/lib/cloudnet）
	rootDir, err := openOrCreateDirectoryPath(root)
	if err != nil {
		return nil, fmt.Errorf("open state root %s: %w", root, err)
	}
	defer rootDir.Close()

	// 第二步：在 root 下打开/创建 networks 子目录
	networksDir, err := openOrCreateDirectoryAt(rootDir, "networks", 0o700)
	if err != nil {
		return nil, fmt.Errorf("open networks directory under %s: %w", root, err)
	}
	defer networksDir.Close()
	if err := networksDir.Chmod(0o700); err != nil {
		return nil, fmt.Errorf("chmod networks directory under %s: %w", root, err)
	}

	// 第三步：在 networks 下打开/创建具体网络名的目录
	networkDir, err := openOrCreateDirectoryAt(networksDir, networkName, 0o700)
	if err != nil {
		return nil, fmt.Errorf("open network state directory %s: %w",
			filepath.Join(root, "networks", networkName), err)
	}
	if err := networkDir.Chmod(0o700); err != nil {
		networkDir.Close()
		return nil, fmt.Errorf("chmod network state directory %s: %w",
			filepath.Join(root, "networks", networkName), err)
	}
	return networkDir, nil
}

// openOrCreateDirectoryPath 从根目录开始，逐组件安全地打开一个绝对路径的目录。
// 拒绝打开文件系统根 / 本身。
func openOrCreateDirectoryPath(path string) (*os.File, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return nil, fmt.Errorf("directory path %q is not absolute", path)
	}
	if clean == string(filepath.Separator) {
		return nil, fmt.Errorf("refuse filesystem root as state root")
	}

	// 从 / 开始
	fd, err := unix.Open(string(filepath.Separator), directoryOpenFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	currentDir := os.NewFile(uintptr(fd), string(filepath.Separator))
	current := string(filepath.Separator)

	// 逐组件打开（如 / → var → lib → cloudnet）
	for _, component := range strings.Split(
		strings.TrimPrefix(clean, string(filepath.Separator)),
		string(filepath.Separator),
	) {
		next, openErr := openOrCreateDirectoryAt(currentDir, component, 0o700)
		_ = currentDir.Close()
		if openErr != nil {
			return nil, fmt.Errorf("open directory component %s: %w",
				filepath.Join(current, component), openErr)
		}
		current = filepath.Join(current, component)
		currentDir = next
	}
	return currentDir, nil
}

// openOrCreateDirectoryAt 在已验证的父目录 fd 下打开或创建一个子目录。
// 使用 openat + O_NOFOLLOW：即使 name 是一个符号链接，也不会跟随。
func openOrCreateDirectoryAt(parent *os.File, name string, mode uint32) (*os.File, error) {
	// 先尝试直接打开（目录已存在的情况）
	fd, err := unix.Openat(int(parent.Fd()), name, directoryOpenFlags, 0)
	if errors.Is(err, unix.ENOENT) {
		// 目录不存在，创建它
		if err := unix.Mkdirat(int(parent.Fd()), name, mode); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("create directory %q: %w", name, err)
		}
		// 创建后再打开
		fd, err = unix.Openat(int(parent.Fd()), name, directoryOpenFlags, 0)
	}
	if err != nil {
		return nil, classifyUnsafeEntry(parent, name, "directory", err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

// openRegularFileAt 在已验证的父目录 fd 下打开一个普通文件。
//
// 额外的安全检查（除 O_NOFOLLOW 外）：
//   - 检查文件类型必须是 S_IFREG（普通文件），不能是设备、FIFO、socket 等
//   - 检查硬链接数必须为 1（没有其他名称指向同一个 inode）
//
// 为什么检查 Nlink==1？
//
//	硬链接攻击：攻击者可以在 /var/lib/cloudnet/networks/cloudnet-v1/ 下
//	创建一个 /etc/shadow 的硬链接。如果 cloudnet 直接写入这个文件，
//	实际上就在修改 /etc/shadow。Nlink==1 保证这不是硬链接。
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

	// fstat 检查文件类型和硬链接数
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

// requireRegularOrAbsentAt 在原子写入前检查目标位置的状态：
//   - 不存在（ENOENT）：可以安全创建
//   - 存在且是普通文件：可以被 rename 覆盖
//   - 存在但是符号链接：拒绝
//   - 存在但不是普通文件：拒绝
func requireRegularOrAbsentAt(parent *os.File, name, displayPath string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil // 文件不存在，可以安全创建
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", displayPath, err)
	}
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		return fmt.Errorf("%s is a symbolic link", displayPath) // 拒绝符号链接
	case unix.S_IFREG:
		return nil // 普通文件，可以覆盖
	default:
		return fmt.Errorf("%s is not a regular file", displayPath)
	}
}

// classifyUnsafeEntry 在打开/创建失败时，检查是否是符号链接导致的，
// 提供更明确的错误信息。
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
