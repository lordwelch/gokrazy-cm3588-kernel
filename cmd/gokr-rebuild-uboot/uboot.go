package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"text/template"
)

const dockerFileContents = `
FROM docker.io/library/debian:trixie

RUN apt-get update && apt-get install -y build-essential crossbuild-essential-arm64 bc libssl-dev bison flex git python3 python3-setuptools swig python3-dev python3-pyelftools uuid-dev libgnutls28-dev
RUN apt-get install -y device-tree-compiler

COPY gokr-build-uboot /usr/bin/gokr-build-uboot
RUN mkdir -p /usr/_src/atf.patches
RUN mkdir -p /usr/_src/uboot.patches
{{- range $idx, $path := .Patches }}
COPY {{ $path }} /usr/_src/{{ $path }}
{{- end }}

RUN echo 'builduser:x:{{ .Uid }}:{{ .Gid }}:nobody:/:/bin/sh' >> /etc/passwd && \
    chown -R {{ .Uid }}:{{ .Gid }} /usr/src /usr/_src

USER builduser
WORKDIR /usr/src
ENV GOKRAZY_IN_DOCKER=1
ENTRYPOINT /usr/bin/gokr-build-uboot
`

var dockerFileTmpl = template.Must(template.New("dockerfile").
	Funcs(map[string]interface{}{
		"basename": func(path string) string {
			return filepath.Base(path)
		},
	}).
	Parse(dockerFileContents))

var ubootPatchFiles = []string{
	"uboot.patches/boot.cmd",
	"uboot.patches/rk3588_bl31_v1.46.elf",
	"uboot.patches/rk3588_ddr_lp4_2112MHz_lp5_2400MHz_v1.16.bin",
	"uboot.patches/rk3588_ddr_lp4_2112MHz_lp5_2400MHz_v1.17.bin",
}

var atfPatchFiles = []string{
	// "atf.patches/feat-rk3588-support-rk3588.patch",
	// "atf.patches/rk3588-enable-crypto-function.patch",
	// "atf.patches/feat-rockchip-support-SCMI-for-clock-reset-domain.patch",
}

func find(filename string) (string, error) {
	if _, err := os.Stat(filename); err == nil {
		return filename, nil
	}

	return "", fmt.Errorf("could not find file %q", filename)
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

func rebuildUboot() {
	overwriteContainerExecutable := flag.String("overwrite_container_executable",
		"",
		"E.g. docker or podman to overwrite the automatically detected container executable")
	persistent := flag.Bool("persistent", false, "Mounts a folder into the docker container to persist u-boot source for debugging")
	flag.Parse()
	executable, err := getContainerExecutable()
	if err != nil {
		log.Fatal(err)
	}

	abs, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSuffix(abs, "/"), "/_build") {
		log.Fatalf("gokr-rebuild-uboot is not run from a _build directory")
	}
	if *overwriteContainerExecutable != "" {
		executable = *overwriteContainerExecutable
	}
	execName := filepath.Base(executable)
	// We explicitly use /tmp, because Docker only allows volume mounts under
	// certain paths on certain platforms, see
	// e.g. https://docs.docker.com/docker-for-mac/osxfs/#namespaces for macOS.
	tmp, err := os.MkdirTemp("/tmp", "gokr-rebuild-uboot")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	exePath, err := os.Executable()
	if err != nil {
		log.Fatal("Unable to find current executable", err)
	}
	buildPath := filepath.Join(tmp, "gokr-build-uboot")
	err = copyFile(buildPath, exePath)
	if err != nil {
		log.Fatal("Unable to copy executable for docker", err)
	}

	var patchPaths []string

	for _, filename := range ubootPatchFiles {
		path, err := find(filename)
		if err != nil {
			log.Fatal(err)
		}
		patchPaths = append(patchPaths, path)
	}

	err = os.MkdirAll(filepath.Join(tmp, "uboot.patches"), 0o750)
	if err != nil {
		log.Fatal(err)
	}
	// Copy all files into the temporary directory so that docker
	// includes them in the build context.
	for _, path := range patchPaths {
		if err = copyFile(filepath.Join(tmp, "uboot.patches", filepath.Base(path)), path); err != nil {
			log.Fatal(err)
		}
	}

	patchPaths = patchPaths[0:0]
	for _, filename := range atfPatchFiles {
		path, err := find(filename)
		if err != nil {
			log.Fatal(err)
		}
		patchPaths = append(patchPaths, path)
	}

	err = os.MkdirAll(filepath.Join(tmp, "atf.patches"), 0o750)
	if err != nil {
		log.Fatal(err)
	}
	// Copy all files into the temporary directory so that docker
	// includes them in the build context.
	for _, path := range patchPaths {
		if err := copyFile(filepath.Join(tmp, "atf.patches", filepath.Base(path)), path); err != nil {
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
		Patches:   append(atfPatchFiles, ubootPatchFiles...),
	}); err != nil {
		log.Fatal(err)
	}

	if err := dockerFile.Close(); err != nil {
		log.Fatal(err)
	}

	log.Printf("building %s container for uboot compilation", execName)

	dockerBuild := exec.Command(executable,
		"build",
		"--rm=true",
		"--tag=gokr-rebuild-uboot",
		".")
	dockerBuild.Dir = tmp
	dockerBuild.Stdout = os.Stdout
	dockerBuild.Stderr = os.Stderr
	if err := dockerBuild.Run(); err != nil {
		log.Fatalf("%s build: %v (cmd: %v)", execName, err, dockerBuild.Args)
	}

	log.Printf("compiling uboot")

	var dockerRun *exec.Cmd
	dockerArgs := []string{
		"run",
		// "--platform=linux/amd64",
		"--volume", tmp + ":/tmp/buildresult:Z",
	}
	if *persistent {
		err = os.MkdirAll("./src_build", 0o777)
		srcBuild, _ := filepath.Abs("./src_build")
		if err != nil {
			log.Fatal("Failed to create ./src_build", err)
		}
		dockerArgs = append(dockerArgs, "-v", srcBuild+":/usr/src")
	} else {
		dockerArgs = append(dockerArgs, fmt.Sprintf("--mount=type=tmpfs,tmpfs-size=%d%s,destination=%s,U", 5, "G", "/usr/src")) // Ramfs for faster build.... maybe
	}
	if execName == "podman" {
		dockerArgs = append(dockerArgs, "--userns=keep-id")
	}
	dockerArgs = append(dockerArgs, "gokr-rebuild-uboot")
	dockerRun = exec.Command(executable, dockerArgs...)
	dockerRun.Dir = tmp
	dockerRun.Stdout = os.Stdout
	dockerRun.Stderr = os.Stderr
	if err := dockerRun.Run(); err != nil {
		log.Fatalf("%s run: %v (cmd: %v)", execName, err, dockerRun.Args)
	}

	for _, filename := range []string{
		"boot.scr",
		"u-boot-rockchip.bin",
	} {
		if err := copyFile(filename, filepath.Join(tmp, filename)); err != nil {
			log.Fatal(err)
		}
	}
}

func main() {
	if os.Getenv("GOKRAZY_IN_DOCKER") == "1" {
		indockerMain()
	} else {
		rebuildUboot()
	}
}
