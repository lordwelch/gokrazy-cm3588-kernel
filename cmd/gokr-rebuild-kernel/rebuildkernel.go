package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"text/template"
)

const dockerFileContents = `
FROM docker.io/library/debian:trixie

RUN apt-get update && apt-get install -y \
{{ if (eq .Cross "arm64") -}}
  crossbuild-essential-arm64 \
{{ end -}}
  build-essential bc libssl-dev bison flex libelf-dev ncurses-dev ca-certificates zstd kmod python3 git

COPY gokr-rebuild-kernel /usr/bin/gokr-rebuild-kernel
COPY config.addendum.txt /usr/_src/config.addendum.txt
COPY defconfig /usr/_src/defconfig
COPY config.addendum.txt /usr/_src/.config
COPY config.addendum.txt /usr/src/.config
{{- range $idx, $path := .Patches }}
COPY patch/{{ $path }} /usr/_src/{{ $path }}
{{- end }}

RUN echo 'builduser:x:{{ .Uid }}:{{ .Gid }}:nobody:/:/bin/sh' >> /etc/passwd && \
    chown -R {{ .Uid }}:{{ .Gid }} /usr/src /usr/_src

USER builduser
WORKDIR /usr/src
ENV GOKRAZY_IN_DOCKER=1
ENTRYPOINT ["/usr/bin/gokr-rebuild-kernel"]
`

var dockerFileTmpl = template.Must(template.New("dockerfile").
	Funcs(map[string]interface{}{
		"basename": func(path string) string {
			return filepath.Base(path)
		},
	}).
	Parse(dockerFileContents))

func copyFile(dest, src string) error {
	log.Printf("copyFile(dest=%s, src=%s)", dest, src)
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

	n, err := io.Copy(out, in)
	if err != nil {
		return err
	}
	log.Printf("  -> %d bytes copied", n)

	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := out.Chmod(st.Mode()); err != nil {
		return err
	}
	return out.Close()
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

func rebuildKernel() error {
	overwriteContainerExecutable := flag.String("overwrite_container_executable",
		"",
		"E.g. docker or podman to overwrite the automatically detected container executable")

	keepBuildContainer := flag.Bool("keep_build_container",
		false,
		"do not delete build container after building the kernel")

	cross := flag.String("cross",
		"arm64",
		"if non-empty, cross-compile for the specified arch (one of 'arm64')")
	persistent := flag.Bool("persistent", false, "Mounts a folder into the docker container to persist kernel source for debugging")

	flavor := flag.String("flavor",
		"vanilla",
		"which kernel flavor to build. one of vanilla (kernel.org) or raspberrypi (https://github.com/raspberrypi/linux/tags)")

	dtbs := flag.String("dtbs",
		"raspberrypi",
		"which device tree files (.dtb files) to copy. 'raspberrypi' or empty")
	_ = dtbs
	flag.Parse()

	if *cross != "" && *cross != "arm64" {
		return fmt.Errorf("invalid -cross value %q: expected one of 'arm64'")
	}

	abs, err := os.Getwd()
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.TrimSuffix(abs, "/"), "/_build") {
		return fmt.Errorf("gokr-rebuild-kernel is not run from a _build directory")
	}

	series, err := os.ReadFile("series")
	if err != nil {
		return err
	}
	patches := strings.Split(strings.TrimSpace(string(series)), "\n")

	executable, err := getContainerExecutable()
	if err != nil {
		return err
	}
	if *overwriteContainerExecutable != "" {
		executable = *overwriteContainerExecutable
	}

	execName := filepath.Base(executable)

	for _, filename := range patches {
		_, err := find("patch/" + filename)
		if err != nil {
			return err
		}
	}

	kernelPath, err := filepath.Abs("../vmlinuz")
	if err != nil {
		return err
	}

	libPath, err := filepath.Abs("../lib")
	if err != nil {
		return err
	}

	if _, err := find("config.addendum.txt"); err != nil {
		return err
	}

	u, err := user.Current()
	if err != nil {
		return err
	}

	upstreamURL, err := os.ReadFile("upstream-url.txt")
	if err != nil {
		return err
	}
	upstreamURL = bytes.TrimSpace(upstreamURL)

	dockerFile, err := os.Create("Dockerfile")
	if err != nil {
		return err
	}

	if err := dockerFileTmpl.Execute(dockerFile, struct {
		Uid     string
		Gid     string
		Patches []string
		Cross   string
	}{
		Uid:     u.Uid,
		Gid:     u.Gid,
		Patches: patches,
		Cross:   *cross,
	}); err != nil {
		return err
	}

	if err := dockerFile.Close(); err != nil {
		return err
	}

	log.Printf("building %s container for kernel compilation", execName)

	dockerBuild := exec.Command(execName,
		"build",
		// "--platform=linux/amd64",
		"--rm=true",
		"--tag=gokr-rebuild-kernel",
		".")
	dockerBuild.Stdout = os.Stdout
	dockerBuild.Stderr = os.Stderr
	log.Printf("%v", dockerBuild.Args)
	if err := dockerBuild.Run(); err != nil {
		return fmt.Errorf("%s build: %v (cmd: %v)", execName, err, dockerBuild.Args)
	}

	log.Printf("compiling kernel")

	var dockerRun *exec.Cmd

	dockerArgs := []string{
		"run",
		// "--platform=linux/amd64",
		"--volume", abs + ":/tmp/buildresult:Z",
	}
	kernelName := path.Base(string(upstreamURL))
	_, err = os.Stat(kernelName)
	log.Printf("Check for downloaded kernel %s: %v", kernelName, err)
	if err == nil {
		absKernelName, _ := filepath.Abs(kernelName)
		dockerArgs = append(dockerArgs, "--volume", absKernelName+":/usr/src/"+kernelName)
	}
	if *persistent {
		err = os.MkdirAll("./src_build", 0o777)
		srcBuild, _ := filepath.Abs("./src_build")
		if err != nil {
			log.Fatal("Failed to create ./src_build", err)
		}
		dockerArgs = append(dockerArgs, "-v", srcBuild+":/usr/src")
		for _, patch := range patches {
			os.Remove(filepath.Join("src_build", filepath.Base(patch)))
		}
	} else {
		dockerArgs = append(dockerArgs, fmt.Sprintf("--mount=type=tmpfs,tmpfs-size=%d%s,destination=%s,U", 5, "G", "/usr/src")) // Ramfs for faster build.... maybe
	}

	if !*keepBuildContainer {
		dockerArgs = append(dockerArgs, "--rm")
	}
	if execName == "podman" {
		dockerArgs = append(dockerArgs, "--userns=keep-id")
	}
	dockerArgs = append(dockerArgs,
		"gokr-rebuild-kernel",
		"-cross="+*cross,
		"-flavor="+*flavor,
		fmt.Sprintf("-persistent=%v", *persistent),
		strings.TrimSpace(string(upstreamURL)))

	dockerRun = exec.Command(executable, dockerArgs...)

	dockerRun.Stdout = os.Stdout
	dockerRun.Stderr = os.Stderr
	log.Printf("%v", dockerRun.Args)
	if err := dockerRun.Run(); err != nil {
		return fmt.Errorf("%s run: %v (cmd: %v)", execName, err, dockerRun.Args)
	}

	if err := copyFile(kernelPath, "vmlinuz"); err != nil {
		return err
	}

	if err := copyFile(kernelPath+".config", "vmlinuz.config"); err != nil {
		return err
	}

	// remove symlinks that only work when source/build directory are present
	for _, subdir := range []string{"build", "source"} {
		matches, err := filepath.Glob(filepath.Join("lib/modules", "*", subdir))
		if err != nil {
			return err
		}
		for _, match := range matches {
			log.Printf("removing build/source symlink %s", match)
			if err := os.Remove(match); err != nil {
				return err
			}
		}
	}

	// replace kernel modules directory
	rm := exec.Command("rm", "-rf", filepath.Join(libPath, "modules"))
	rm.Stdout = os.Stdout
	rm.Stderr = os.Stderr
	log.Printf("%v", rm.Args)
	if err := rm.Run(); err != nil {
		return fmt.Errorf("%v: %v", rm.Args, err)
	}
	cp := exec.Command("cp", "-r", filepath.Join("lib/modules"), libPath)
	cp.Stdout = os.Stdout
	cp.Stderr = os.Stderr
	log.Printf("%v", cp.Args)
	if err := cp.Run(); err != nil {
		return fmt.Errorf("%v: %v", cp.Args, err)
	}

	if *cross == "arm64" {
		if *flavor == "raspberrypi" {
			// replace overlays directory
			overlaysPath, err := find("../overlays")
			if err != nil {
				return err
			}
			rm = exec.Command("rm", "-rf", overlaysPath)
			rm.Stdout = os.Stdout
			rm.Stderr = os.Stderr
			log.Printf("%v", rm.Args)
			if err := rm.Run(); err != nil {
				log.Printf("%v: %v", rm.Args, err)
			}
			cp = exec.Command("cp", "-r", "overlays", overlaysPath)
			cp.Stdout = os.Stdout
			cp.Stderr = os.Stderr
			log.Printf("%v", cp.Args)
			if err := cp.Run(); err != nil {
				return fmt.Errorf("%v: %v", cp.Args, err)
			}
		}
	}

	return nil
}

func main() {
	if os.Getenv("GOKRAZY_IN_DOCKER") == "1" {
		indockerMain()
	} else {
		if err := rebuildKernel(); err != nil {
			log.Fatal(err)
		}
	}
}
