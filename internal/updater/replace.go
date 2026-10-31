package updater

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ApplyUpdate 将已校验的新版本二进制文件替换到目标可执行文件位置。
// targetExecPath 为空时自动获取当前正在运行的程序真实路径。
func ApplyUpdate(downloadedPath, targetExecPath string) error {
	var err error
	if targetExecPath == "" {
		targetExecPath, err = os.Executable()
		if err != nil {
			return fmt.Errorf("locate current executable: %w", err)
		}
		if realPath, err := filepath.EvalSymlinks(targetExecPath); err == nil && realPath != "" {
			targetExecPath = realPath
		}
	}

	targetDir := filepath.Dir(targetExecPath)
	targetName := filepath.Base(targetExecPath)

	if runtime.GOOS == "windows" {
		// Windows: 运行中 exe 不可直接写覆盖，但可重命名。
		oldPath := filepath.Join(targetDir, targetName+".old")
		newTmpPath := filepath.Join(targetDir, targetName+".new")
		_ = os.Remove(newTmpPath)

		// 1. 先将下载文件移动/复制到同盘临时文件 targetName.new
		if err := moveOrCopy(downloadedPath, newTmpPath); err != nil {
			return fmt.Errorf("stage new binary to %s: %w", newTmpPath, err)
		}

		// 2. 将正在运行的旧文件重命名为 targetName.old
		_ = os.Remove(oldPath) // 若存在历史 .old 则清理
		if err := os.Rename(targetExecPath, oldPath); err != nil {
			_ = os.Remove(newTmpPath)
			return fmt.Errorf("rename running binary to .old: %w", err)
		}

		// 3. 将新文件重命名为 targetName
		if err := os.Rename(newTmpPath, targetExecPath); err != nil {
			// 发生异常，回滚恢复旧二进制
			rollbackErr := os.Rename(oldPath, targetExecPath)
			_ = os.Remove(newTmpPath)
			if rollbackErr != nil {
				return fmt.Errorf("replace binary failed (%v) and rollback also failed: %w", err, rollbackErr)
			}
			return fmt.Errorf("replace binary failed (rolled back): %w", err)
		}
	} else {
		// Linux / macOS: 使用同目录隐藏临时文件原子 Rename 替换
		tmpTarget := filepath.Join(targetDir, "."+targetName+".new")
		_ = os.Remove(tmpTarget)

		if err := moveOrCopy(downloadedPath, tmpTarget); err != nil {
			return fmt.Errorf("stage new binary: %w", err)
		}

		// 赋予可执行权限 0755
		if err := os.Chmod(tmpTarget, 0755); err != nil {
			_ = os.Remove(tmpTarget)
			return fmt.Errorf("chmod new binary: %w", err)
		}

		if err := os.Rename(tmpTarget, targetExecPath); err != nil {
			_ = os.Remove(tmpTarget)
			return fmt.Errorf("atomic rename new binary: %w", err)
		}
		_ = os.Chmod(targetExecPath, 0755)
	}

	_ = os.Remove(downloadedPath)
	return nil
}

// CleanupOldFiles 清理旧版残留的 .old 文件。在程序启动并稳定运行后调用。
func CleanupOldFiles() {
	execPath, err := os.Executable()
	if err != nil {
		return
	}
	if realPath, err := filepath.EvalSymlinks(execPath); err == nil && realPath != "" {
		execPath = realPath
	}

	oldPath := execPath + ".old"
	if fi, err := os.Stat(oldPath); err == nil && !fi.IsDir() {
		_ = os.Remove(oldPath)
	}
}

// LaunchProcess 启动新进程。
func LaunchProcess(execPath string, args []string) error {
	if execPath == "" {
		var err error
		execPath, err = os.Executable()
		if err != nil {
			return fmt.Errorf("locate executable: %w", err)
		}
		if realPath, err := filepath.EvalSymlinks(execPath); err == nil && realPath != "" {
			execPath = realPath
		}
	}

	cmd := exec.Command(execPath, args...)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	return cmd.Start()
}

func moveOrCopy(src, dst string) error {
	// 首先尝试原子重命名（同分区快速移动）
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	// 跨盘/跨分区时回退到安全复制：写临时文件后重命名，严禁直接截断 dst！
	dstTmp := dst + ".part"
	_ = os.Remove(dstTmp)

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dstTmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0755)
	if err != nil {
		return err
	}

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		_ = dstFile.Close()
		_ = os.Remove(dstTmp)
		return err
	}
	if err := dstFile.Sync(); err != nil {
		_ = dstFile.Close()
		_ = os.Remove(dstTmp)
		return err
	}
	if err := dstFile.Close(); err != nil {
		_ = os.Remove(dstTmp)
		return err
	}

	if err := os.Rename(dstTmp, dst); err != nil {
		_ = os.Remove(dstTmp)
		return err
	}

	_ = os.Remove(src)
	return nil
}
