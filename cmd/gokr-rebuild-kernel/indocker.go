package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

var (
	firmware       = []string{"arm/mali/arch10.8/mali_csffw.bin", "rtl_nic/rtl8125b-2.fw"}
	firmwareDir, _ = filepath.Abs("firmware")
)

func downloadKernel(latest string) error {
	if _, err := os.Stat(filepath.Base(latest)); err == nil {
		return nil
	}
	out, err := os.Create(filepath.Base(latest))
	if err != nil {
		return err
	}
	defer out.Close()
	if kernel, err := os.Open("/tmp/buildresult/" + filepath.Base(latest)); err == nil {
		defer kernel.Close()
		if _, err := io.Copy(out, kernel); err != nil {
			return err
		}
		return out.Close()
	}
	resp, err := http.Get(latest)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return fmt.Errorf("unexpected HTTP status code for %s: got %d, want %d", latest, got, want)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}
	return out.Close()
}

func downloadWhence() (map[string]string, error) {
	whenceMap := make(map[string]string)
	uri := "https://gitlab.com/api/v4/projects/48890189/repository/files/WHENCE/raw?ref=main"
	resp, err := http.Get(uri)
	if err != nil {
		return whenceMap, err
	}
	defer resp.Body.Close()
	whenceBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return whenceMap, err
	}
	whence := strings.Split(string(whenceBytes), "\n")
	for _, line := range whence {
		if file, ok := strings.CutPrefix(line, "File: "); ok {
			whenceMap[file] = file
		}
		if file, ok := strings.CutPrefix(line, "RawFile: "); ok {
			whenceMap[file] = file
		}
		if l, ok := strings.CutPrefix(line, "Link: "); ok {
			if link, file, ok := strings.Cut(l, " -> "); ok {
				dest := filepath.Join(filepath.Dir(link), file)
				whenceMap[link] = dest
			}
		}
	}
	return whenceMap, nil
}

func downloadFirmware() ([]string, error) {
	firmwarePaths := make([]string, 0, len(firmware))
	whence, err := downloadWhence()
	if err != nil {
		return firmwarePaths, err
	}
	for _, f := range firmware {
		folder := filepath.Dir(f)
		path, ok := whence[f]
		if !ok {
			return firmwarePaths, fmt.Errorf("firmware %q not found", f)
		}

		firmwarePaths = append(firmwarePaths, f)

		log.Printf("downloading firmware: %q to %q", path, f)

		os.MkdirAll(filepath.Join("firmware", folder), 0o777)
		out, err := os.Create(filepath.Join("firmware", f))
		if err != nil {
			return firmwarePaths, err
		}
		defer out.Close()
		uri := "https://gitlab.com/api/v4/projects/48890189/repository/files/" + url.PathEscape(path) + "/raw?ref=main"
		resp, err := http.Get(uri)
		if err != nil {
			return firmwarePaths, err
		}
		defer resp.Body.Close()
		if got, want := resp.StatusCode, http.StatusOK; got != want {
			return firmwarePaths, fmt.Errorf("unexpected HTTP status code for %s: got %d, want %d", uri, got, want)
		}
		if _, err := io.Copy(out, resp.Body); err != nil {
			return firmwarePaths, err
		}
		if err := out.Close(); err != nil {
			return firmwarePaths, err
		}
		resp.Body.Close()
	}
	return firmwarePaths, nil
}

func applyPatches(srcdir string) error {
	patches, err := filepath.Glob("*.patch")
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

func compile(cross, flavor string, firmwarePaths []string) error {
	defconfig := exec.Command("make", "ARCH="+os.Getenv("ARCH"), "defconfig")
	if flavor == "defconfig" {
		defconfig = exec.Command("make", "ARCH="+os.Getenv("ARCH"), "olddefconfig")
		cpConfig := exec.Command("cp", "/usr/_src/defconfig", ".config")
		cpConfig.Stdout = os.Stdout
		cpConfig.Stderr = os.Stderr
		if err := cpConfig.Run(); err != nil {
			return fmt.Errorf("make cpConfig: %v", err)
		}
	} else if flavor == "raspberrypi" {
		// TODO(https://github.com/gokrazy/gokrazy/issues/223): is it
		// necessary/desirable to switch to bcm2712_defconfig?
		defconfig = exec.Command("make", "ARCH=arm64", "bcm2711_defconfig")
	} else if strings.HasSuffix(flavor, "_defconfig") {
		defconfig = exec.Command("make", "ARCH="+os.Getenv("ARCH"), flavor)
	}

	defconfig.Stdout = os.Stdout
	defconfig.Stderr = os.Stderr
	if err := defconfig.Run(); err != nil {
		return fmt.Errorf("make defconfig: %v", err)
	}

	// Change answers from mod to no if possible, i.e. disable all modules so
	// that we end up with a minimal set of modules (from the config addendum).
	mod2noconfig := exec.Command("make", "mod2noconfig")
	mod2noconfig.Stdout = os.Stdout
	mod2noconfig.Stderr = os.Stderr
	if err := mod2noconfig.Run(); err != nil {
		return fmt.Errorf("make mod2noconfig: %v", err)
	}

	f, err := os.OpenFile(".config", os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	addendum, err := os.ReadFile("/usr/src/config.addendum.txt")
	if err != nil {
		return err
	}
	if _, err := f.Write(addendum); err != nil {
		return err
	}
	if len(firmwarePaths) > 0 {
		fmt.Fprintf(f, "CONFIG_EXTRA_FIRMWARE=%q\n", strings.Join(firmwarePaths, " "))
		fmt.Fprintf(f, "CONFIG_EXTRA_FIRMWARE_DIR=%q\n", firmwareDir)
	}

	if err := f.Close(); err != nil {
		return err
	}

	olddefconfig := exec.Command("make", "olddefconfig")
	olddefconfig.Stdout = os.Stdout
	olddefconfig.Stderr = os.Stderr
	if err := olddefconfig.Run(); err != nil {
		return fmt.Errorf("make olddefconfig: %v", err)
	}

	env := append(os.Environ(),
		"KBUILD_BUILD_USER=gokrazy",
		"KBUILD_BUILD_HOST=docker",
		"KBUILD_BUILD_TIMESTAMP=Wed Mar  1 20:57:29 UTC 2017",
	)
	make := exec.Command("make", "bzImage", "modules", "-j"+strconv.Itoa(runtime.NumCPU()))
	if cross == "arm64" {
		make = exec.Command("make", "Image.gz", "dtbs", "modules", "-j"+strconv.Itoa(runtime.NumCPU()))
	}
	make.Env = env
	make.Stdout = os.Stdout
	make.Stderr = os.Stderr
	if err := make.Run(); err != nil {
		return fmt.Errorf("make: %v", err)
	}

	make = exec.Command("make", "INSTALL_MOD_PATH=/tmp/buildresult", "modules_install", "-j"+strconv.Itoa(runtime.NumCPU()))
	make.Env = env
	make.Stdout = os.Stdout
	make.Stderr = os.Stderr
	if err := make.Run(); err != nil {
		return fmt.Errorf("make: %v", err)
	}

	make = exec.Command("make", "INSTALL_DTBS_PATH=/tmp/buildresult/dtbs", "dtbs_install", "-j"+strconv.Itoa(runtime.NumCPU()))
	make.Env = env
	make.Stdout = os.Stdout
	make.Stderr = os.Stderr
	if err := make.Run(); err != nil {
		return fmt.Errorf("make: %v", err)
	}

	return nil
}

func indockerMain() {
	cross := flag.String("cross",
		"",
		"if non-empty, cross-compile for the specified arch (one of 'arm64')")

	flavor := flag.String("flavor",
		"vanilla",
		"which kernel flavor to build. one of vanilla (kernel.org) or raspberrypi (https://github.com/raspberrypi/linux/tags)")
	persistent := flag.Bool("persistent", false, "Mounts a folder into the docker container to persist kernel source for debugging")

	flag.Parse()
	latest := flag.Arg(0)
	if latest == "" {
		log.Fatalf("syntax: %s <upstream-URL>", os.Args[0])
	}
	log.Printf("downloading kernel source: %s", latest)
	err := downloadKernel(latest)
	if err != nil {
		log.Fatal(err)
	}

	srcdir := strings.TrimSuffix(filepath.Base(latest), ".tar.gz")
	srcdir = strings.TrimSuffix(srcdir, ".tar.xz")
	if *flavor != "vanilla" && strings.HasPrefix(latest, "https://github.com/") {
		s := strings.SplitN(latest, "/", 6)
		if len(s) < 6 {
			srcdir = "linux-" + srcdir
		} else {
			srcdir = s[4] + "-" + srcdir
		}
	}

	if *persistent {
		if _, err = os.Stat(srcdir); err == nil {
			err = os.ErrExist
		} else {
			err = nil
		}
	}
	unpacked := false
	if err == nil {
		log.Printf("unpacking kernel source")
		untar := exec.Command("tar", "xf", filepath.Base(latest))
		untar.Stdout = os.Stdout
		untar.Stderr = os.Stderr
		if err := untar.Run(); err != nil {
			log.Fatalf("untar: %v", err)
		}
		unpacked = true
	}
	srcFiles, err := filepath.Glob("/usr/_src/*")
	if err != nil {
		log.Fatalf("failed to find source files: %v", err)
	}
	for _, fileName := range srcFiles {
		file, err := os.OpenFile(fileName, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			log.Fatalf("Unable to open source file for writing: %s", filepath.Base(fileName))
		}
		newFile, err := os.OpenFile(filepath.Base(fileName), os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			log.Fatalf("Unable to open source file for writing: %s", filepath.Base(fileName))
		}
		_, err = io.Copy(newFile, file)
		if err != nil {
			log.Fatalf("Error when copying file %v: %v", fileName, err)
		}
		file.Close()
		newFile.Close()
	}

	log.Printf("applying patches")
	if unpacked {
		if err := applyPatches(srcdir); err != nil {
			log.Fatal(err)
		}
	}
	firmwarePaths, err := downloadFirmware()
	if err != nil {
		log.Fatal(err)
	}

	if err := os.Chdir(srcdir); err != nil {
		log.Fatal(err)
	}

	if *cross == "arm64" {
		log.Printf("exporting ARCH=arm64, CROSS_COMPILE=aarch64-linux-gnu-")
		os.Setenv("ARCH", "arm64")
		os.Setenv("CROSS_COMPILE", "aarch64-linux-gnu-")
	}

	log.Printf("compiling kernel")
	if err := compile(*cross, *flavor, firmwarePaths); err != nil {
		log.Fatal(err)
	}

	if *cross == "arm64" {
		if err := copyFile("/tmp/buildresult/vmlinuz", "arch/arm64/boot/Image"); err != nil {
			log.Fatal(err)
		}
		if err := copyFile("/tmp/buildresult/vmlinuz.config", ".config"); err != nil {
			log.Fatal(err)
		}

		dtbos, err := filepath.Glob("arch/arm64/boot/dts/overlays/*.dtbo")
		if err != nil {
			log.Fatal(err)
		}
		if _, err = os.Stat("arch/arm64/boot/dts/overlays/overlay_map.dtb"); err == nil {
			dtbos = append(dtbos, "arch/arm64/boot/dts/overlays/overlay_map.dtb")
		}
		if err := os.MkdirAll("/tmp/buildresult/overlays", 0o755); err != nil {
			log.Fatal(err)
		}
		for _, fn := range dtbos {
			if err := copyFile(filepath.Join("/tmp/buildresult/overlays/", filepath.Base(fn)), fn); err != nil {
				log.Fatal(err)
			}
		}
	} else {
		if err := copyFile("/tmp/buildresult/vmlinuz", "arch/x86/boot/bzImage"); err != nil {
			log.Fatal(err)
		}
	}
}
