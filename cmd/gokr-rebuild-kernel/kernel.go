package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"text/template"
)

const dockerFileContents = `
FROM debian:bullseye

RUN apt-get update && apt-get install -y crossbuild-essential-arm64 bc libssl-dev bison flex
RUN mkdir -p /usr/src/kernel.patches

COPY gokr-build-kernel /usr/bin/gokr-build-kernel
{{- range $idx, $path := .Patches }}
COPY {{ $path }} /usr/src/{{ $path }}
{{- end }}

RUN echo 'builduser:x:{{ .Uid }}:{{ .Gid }}:nobody:/:/bin/sh' >> /etc/passwd && \
    chown -R {{ .Uid }}:{{ .Gid }} /usr/src

USER builduser
WORKDIR /usr/src
ENTRYPOINT /usr/bin/gokr-build-kernel
`

var dockerFileTmpl = template.Must(template.New("dockerfile").
	Funcs(map[string]interface{}{
		"basename": func(path string) string {
			return filepath.Base(path)
		},
	}).
	Parse(dockerFileContents))

var patchFiles = []string{
	"kernel.patches/0001-dt-bindings-arm-rockchip-Add-FriendlyElec-CM3588-NAS.patch",
	"kernel.patches/0002-arm64-dts-rockchip-Add-FriendlyElec-CM3588-NAS-board.patch",
	"kernel.patches/0010-fix-clk-divisions.patch",
	"kernel.patches/0011-irqchip-fix-its-timeout-issue.patch",
	"kernel.patches/0012-fix-initial-PERST-GPIO-value.patch",
	"kernel.patches/0022-RK3588-Add-Thermal-and-CpuFreq-Support.patch",
	"kernel.patches/0024-RK3588-Add-Crypto-Support.patch",
	"kernel.patches/0025-RK3588-Add-HW-RNG-Support.patch",
	"kernel.patches/0026-RK3588-Add-VPU121-H.264-Decoder-Support.patch",
	"kernel.patches/0027-RK3588-Add-rkvdec2-Support-v3.patch",
	"kernel.patches/0028-media-v4l2-core-Initialize-h264-frame_mbs_only_flag-.patch",
	"kernel.patches/0138-arm64-dts-rockchip-Add-HDMI0-bridge-to-rk3588.patch",
	"kernel.patches/0139-arm64-dts-rockchip-Enable-HDMI0-PHY-clk-provider-on-.patch",
	"kernel.patches/0144-phy-phy-rockchip-samsung-hdptx-Add-FRL-EARC-support.patch",
	"kernel.patches/0145-phy-phy-rockchip-samsung-hdptx-Add-clock-provider.patch",
	"kernel.patches/0146-drm-rockchip-vop2-Improve-display-modes-handling-on-.patch",
	"kernel.patches/0147-arm64-dts-rockchip-rk3588-add-RGA2-node.patch",
	"kernel.patches/0161-drm-bridge-synopsys-Add-initial-support-for-DW-HDMI-Controller.patch",
	"kernel.patches/0162-drm-bridge-synopsys-Fix-HDMI-Controller.patch",
	"kernel.patches/0170-drm-rockchip-vop2-add-clocks-reset-support.patch",
	"kernel.patches/0801-wireless-add-bcm43752.patch",
	"kernel.patches/0802-wireless-add-clk-property.patch",
	"kernel.patches/1010-arm64-dts-rock-5b-Slow-down-emmc-to-hs200-and-add-ts.patch",
	"kernel.patches/1011-board-rock-5b-arm64-dts-enable-spi-flash.patch",
	"kernel.patches/1012-arm64-dts-rockchip-Enable-HDMI0-on-rock-5b.patch",
	"kernel.patches/1014-arm64-dts-rockchip-Make-use-of-HDMI0-PHY-PLL-on-rock5b.patch",
	"kernel.patches/1015-board-rock5b-automatic-fan-control.patch",
	"kernel.patches/1020-Add-HDMI-and-VOP2-to-Rock-5A.patch",
	"kernel.patches/1021-arch-arm64-dts-enable-gpu-node-for-rock-5a.patch",
	"kernel.patches/1021-arm64-dts-Add-missing-nodes-to-Orange-Pi-5-Plus.patch",
	"kernel.patches/1023-arm64-dts-rockchip-add-PCIe-for-M.2-E-Key-to-rock-5a.patch",
	"kernel.patches/1031-arm64-dts-rockchip-Add-HDMI-support-to-ArmSoM-Sige7.patch",
	"kernel.patches/1032-arm64-dts-rockchip-Add-ap6275p-wireless-support-to-A.patch",
	"kernel.patches/1040-board-khadas-edge2-add-nodes.patch",
	"kernel.patches/1041-board-khadas-edge2-mcu.patch",
	"kernel.patches/1051-arm64-dts-rockchip-Add-NanoPC-T6-SPI-Flash.patch",
	// "linux-6.10-rc7.tar.gz",
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

var gopath = mustGetGopath()

func mustGetGopath() string {
	gopathb, err := exec.Command("go", "env", "GOPATH").Output()
	if err != nil {
		log.Panic(err)
	}
	return strings.TrimSpace(string(gopathb))
}

func find(filename string) (string, error) {
	if _, err := os.Stat(filename); err == nil {
		return filename, nil
	}

	path := filepath.Join(gopath, "src", "gitea.narnian.us", "lordwelch", "gokrazy-cm3588-kernel", filename)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("could not find file %q (looked in . and %s)", filename, path)
}

func getContainerExecutable() (string, error) {
	// Probe podman first, because the docker binary might actually
	// be a thin podman wrapper with podman behavior.
	choices := []string{"podman", "docker"}
	for _, exe := range choices {
		p, err := exec.LookPath(exe)
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", err
		}
		return resolved, nil
	}
	return "", fmt.Errorf("none of %v found in $PATH", choices)
}

func main() {
	var overwriteContainerExecutable = flag.String("overwrite_container_executable",
		"",
		"E.g. docker or podman to overwrite the automatically detected container executable")
	flag.Parse()
	executable, err := getContainerExecutable()
	if err != nil {
		log.Fatal(err)
	}
	if *overwriteContainerExecutable != "" {
		executable = *overwriteContainerExecutable
	}
	execName := filepath.Base(executable)
	// We explicitly use /tmp, because Docker only allows volume mounts under
	// certain paths on certain platforms, see
	// e.g. https://docs.docker.com/docker-for-mac/osxfs/#namespaces for macOS.
	tmp, err := ioutil.TempDir("/tmp", "gokr-rebuild-kernel")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	cmd := exec.Command("go", "build", "-o", tmp, "gitea.narnian.us/lordwelch/gokrazy-cm3588-kernel/cmd/gokr-build-kernel")
	cmd.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("%v: %v", cmd.Args, err)
	}

	buildPath := filepath.Join(tmp, "gokr-build-kernel")

	var patchPaths []string
	for _, filename := range patchFiles {
		path, err := find(filename)
		if err != nil {
			log.Fatal(err)
		}
		patchPaths = append(patchPaths, path)
	}

	kernelPath, err := find("vmlinuz")
	if err != nil {
		log.Fatal(err)
	}
	dtbPath, err := find("rk3588-friendlyelec-cm3588-nas.dtb")
	if err != nil {
		log.Fatal(err)
	}
	err = os.MkdirAll(filepath.Join(tmp, "kernel.patches"), 0750)
	if err != nil {
		log.Fatal(err)
	}
	// Copy all files into the temporary directory so that docker
	// includes them in the build context.
	for _, path := range patchPaths {
		if err := copyFile(filepath.Join(tmp, "kernel.patches", filepath.Base(path)), path); err != nil {
			log.Fatal(err)
		}
	}

	u, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	dockerFile, err := os.Create(filepath.Join(tmp, "Dockerfile"))
	if err != nil {
		log.Fatal(err)
	}

	if err := dockerFileTmpl.Execute(dockerFile, struct {
		Uid       string
		Gid       string
		BuildPath string
		Patches   []string
	}{
		Uid:       u.Uid,
		Gid:       u.Gid,
		BuildPath: buildPath,
		Patches:   patchFiles,
	}); err != nil {
		log.Fatal(err)
	}

	if err := dockerFile.Close(); err != nil {
		log.Fatal(err)
	}

	log.Printf("building %s container for kernel compilation", execName)

	dockerBuild := exec.Command(execName,
		"build",
		"--rm=true",
		"--tag=gokr-rebuild-kernel",
		".")
	dockerBuild.Dir = tmp
	dockerBuild.Stdout = os.Stdout
	dockerBuild.Stderr = os.Stderr
	if err := dockerBuild.Run(); err != nil {
		log.Fatalf("%s build: %v (cmd: %v)", execName, err, dockerBuild.Args)
	}

	log.Printf("compiling kernel")

	var dockerRun *exec.Cmd
	if execName == "podman" {
		dockerRun = exec.Command(executable,
			"run",
			"--userns=keep-id",
			"--rm",
			"--volume", tmp+":/tmp/buildresult:Z",
			"gokr-rebuild-kernel")
	} else {
		dockerRun = exec.Command(executable,
			"run",
			"--rm",
			"--volume", tmp+":/tmp/buildresult:Z",
			"gokr-rebuild-kernel")
	}
	dockerRun.Dir = tmp
	dockerRun.Stdout = os.Stdout
	dockerRun.Stderr = os.Stderr
	if err := dockerRun.Run(); err != nil {
		log.Fatalf("%s run: %v (cmd: %v)", execName, err, dockerRun.Args)
	}

	if err := copyFile(kernelPath, filepath.Join(tmp, "vmlinuz")); err != nil {
		log.Fatal(err)
	}

	if err := copyFile(dtbPath, filepath.Join(tmp, "rk3588-friendlyelec-cm3588-nas.dtb")); err != nil {
		log.Fatal(err)
	}
}
