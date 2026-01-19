package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	uBootRepo           = "https://github.com/u-boot/u-boot"
	ubootRev            = "ff498a3c5efb424accc1d825cc45cede2540ca13"
	trustedFirmwareRepo = "https://github.com/ARM-software/arm-trusted-firmware"
	trustedRepoRev      = "6251d6ed1ffa7080edc55fa75f525e19ecf5edbd"
)

func applyPatches(srcdir, t string) error {
	patches, err := filepath.Glob(t + ".patches/*.patch")
	if err != nil {
		return err
	}
	for _, patch := range patches {
		log.Printf("applying patch %q", patch)
		f, err := os.Open(patch)
		if err != nil {
			return err
		}
		defer f.Close()
		cmd := exec.Command("patch", "-p1")
		cmd.Dir = srcdir
		cmd.Stdin = f
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		f.Close()
	}

	return nil
}

func compile(trustedFirmwareDir string) error {
	defconfig := exec.Command("make", "ARCH=arm64", "cm3588-nas-rk3588_defconfig")
	defconfig.Stdout = os.Stdout
	defconfig.Stderr = os.Stderr
	if err := defconfig.Run(); err != nil {
		return fmt.Errorf("make defconfig: %v", err)
	}

	f, err := os.OpenFile(".config", os.O_RDWR|os.O_APPEND, 0o755)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(`
CONFIG_CMD_SETEXPR=y
CONFIG_CMD_SETEXPR_FMT=y
CONFIG_BOOTCOMMAND="setenv bootmeths script; bootflow scan -lb"
`)); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	cmd := exec.Command("git", "show", "-s", "--date=unix", "--pretty=format:%ad", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("unable to get date of git repo: %w", err)
	}

	make := exec.Command("make", "-j"+strconv.Itoa(runtime.NumCPU()))
	make.Env = append(os.Environ(),
		"ARCH=arm64",
		"CROSS_COMPILE=aarch64-linux-gnu-",
		"SOURCE_DATE_EPOCH="+strings.TrimSpace(string(output)),
		fmt.Sprintf("BL31=%s/build/rk3588/release/bl31/bl31.elf", trustedFirmwareDir),
		fmt.Sprintf("ROCKCHIP_TPL=%s", "/usr/src/uboot.patches/rk3588_ddr_lp4_2112MHz_lp5_2400MHz_v1.16.bin"),
	)
	make.Stdout = os.Stdout
	make.Stderr = os.Stderr
	if err := make.Run(); err != nil {
		return fmt.Errorf("make: %v", err)
	}

	return nil
}

func generateBootScr(bootCmdPath string) error {
	mkimage := exec.Command("./tools/mkimage", "-A", "arm", "-T", "script", "-C", "none", "-d", bootCmdPath, "boot.scr")
	mkimage.Env = append(os.Environ(),
		"ARCH=arm64",
		"CROSS_COMPILE=aarch64-linux-gnu-",
		"SOURCE_DATE_EPOCH=1600000000",
	)
	mkimage.Stdout = os.Stdout
	mkimage.Stderr = os.Stderr
	if err := mkimage.Run(); err != nil {
		return fmt.Errorf("mkimage: %v", err)
	}

	return nil
}

func copyFile(dest, src string) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := out.Chmod(st.Mode()); err != nil {
		return err
	}
	return out.Close()
}

func clone(dir string, repo string, rev string) error {


	for _, cmd := range [][]string{
		{"git", "init"},
		{"git", "remote", "add", "origin", repo},
		{"git", "fetch", "--depth=1", "origin", rev},
		{"git", "checkout", "FETCH_HEAD"},
	} {
		log.Printf("Running %s", cmd)
		cmdObj := exec.Command(cmd[0], cmd[1:]...)
		cmdObj.Stdout = os.Stdout
		cmdObj.Stderr = os.Stderr
		cmdObj.Dir = dir
		if err := cmdObj.Run(); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	ubootDir, err := os.MkdirTemp("", "u-boot")
	if err != nil {
		log.Fatal(err)
	}

	trustedFirmwareDir, err := os.MkdirTemp("", "arm-trusted-firmware")
	if err != nil {
		log.Fatal(err)
	}

	if err = clone(trustedFirmwareDir,trustedFirmwareDir,trustedRepoRev); err != nil {
		log.Fatal("Failed to clone Trusted Firmware:", err)
	}

	log.Printf("applying patches")
	if err := applyPatches(trustedFirmwareDir, "atf"); err != nil {
		log.Fatal(err)
	}
	for _, cmd := range [][]string{
		{"make", "SOURCE_DATE_EPOCH=1600000000", "CROSS_COMPILE=aarch64-linux-gnu-", "PLAT=rk3588"},
	} {
		log.Printf("Running %s", cmd)
		cmdObj := exec.Command(cmd[0], cmd[1:]...)
		cmdObj.Stdout = os.Stdout
		cmdObj.Stderr = os.Stderr
		cmdObj.Dir = trustedFirmwareDir
		if err := cmdObj.Run(); err != nil {
			log.Fatal(err)
		}
	}

	var bootCmdPath string
	if p, err := filepath.Abs("uboot.patches/boot.cmd"); err != nil {
		log.Fatal(err)
	} else {
		bootCmdPath = p
	}

	if err := os.Chdir(ubootDir); err != nil {
		log.Fatal(err)
	}

	if err = clone(ubootDir, uBootRepo, ubootRev); err != nil {
		log.Fatal("Failed to clone uboot repo:", err)
	}

	log.Printf("applying patches")
	if err := applyPatches(ubootDir, "uboot"); err != nil {
		log.Fatal(err)
	}

	log.Printf("compiling uboot")
	if err := compile(trustedFirmwareDir); err != nil {
		log.Fatal(err)
	}

	log.Printf("generating boot.scr")
	if err := generateBootScr(bootCmdPath); err != nil {
		log.Fatal(err)
	}

	for _, copyCfg := range []struct {
		dest, src string
	}{
		{"boot.scr", "boot.scr"},
		{"u-boot-rockchip.bin", "u-boot-rockchip.bin"},
	} {
		if err := copyFile(filepath.Join("/tmp/buildresult", copyCfg.dest), copyCfg.src); err != nil {
			log.Fatal(err)
		}
	}
}
