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

	kernelPath, err := find("../vmlinuz")
	if err != nil {
		return err
	}

	libPath, err := find("../lib")
	if err != nil {
		return err
	}

	// TODO: just ensure the file exists, i.e. we are in _build
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
		os.MkdirAll("./src_build", 0o777)
		dockerArgs = append(dockerArgs, "-v", "./src_build:/usr/src")
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
		// downloadFirmware()
		if *dtbs != "" {
			// replace device tree files
			rm = exec.Command("sh", "-c", "rm -f ../*.dtb")
			rm.Stdout = os.Stdout
			rm.Stderr = os.Stderr
			log.Printf("%v", rm.Args)
			if err := rm.Run(); err != nil {
				log.Printf("%v: %v", rm.Args, err)
			}
			cp = exec.Command("sh", "-c", "cp *.dtb ..")
			cp.Stdout = os.Stdout
			cp.Stderr = os.Stderr
			log.Printf("%v", cp.Args)
			if err := cp.Run(); err != nil {
				return fmt.Errorf("%v: %v", cp.Args, err)
			}
		}

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

// func _downloadFirmware() (*os.File, int64, error) {
// 	latest := "https://gitlab.com/freedesktop-sdk/mirrors/kernel/linux/kernel/git/firmware/linux-firmware/-/raw/main/arm/mali/arch10.8/mali_csffw.bin"
// 	if st, err := os.Stat(filepath.Base(latest)); err == nil {
// 		out, err := os.Open(filepath.Base(latest))
// 		if err != nil {
// 			return nil, 0, nil
// 		}
// 		return out, st.Size(), nil
// 	}
// 	out, err := os.Create(filepath.Base(latest))
// 	if err != nil {
// 		return nil, 0, err
// 	}
// 	resp, err := http.Get(latest)
// 	if err != nil {
// 		out.Close()
// 		return out, 0, err
// 	}
// 	defer resp.Body.Close()
// 	if got, want := resp.StatusCode, http.StatusOK; got != want {
// 		out.Close()
// 		return out, 0, fmt.Errorf("unexpected HTTP status code for %s: got %d, want %d", latest, got, want)
// 	}
// 	size, err := io.Copy(out, resp.Body)
// 	if err != nil {
// 		out.Close()
// 		return out, 0, err
// 	}
// 	if _, err := out.Seek(0, os.SEEK_SET); err != nil {
// 		out.Close()
// 		return out, 0, err
// 	}
// 	return out, size, nil
// }

// func downloadFirmware() error {
// 	firmwareFile, size, err := _downloadFirmware()
// 	if err != nil {
// 		return err
// 	}
// 	defer firmwareFile.Close()
// 	err = os.MkdirAll("../_gokrazy", os.ModePerm)
// 	if err != nil {
// 		return err
// 	}
// 	f, err := os.Create("../_gokrazy/extrafiles.tar")
// 	if err != nil {
// 		return err
// 	}
// 	defer f.Close()
// 	t := tar.NewWriter(f)
// 	if err := t.WriteHeader(&tar.Header{
// 		Name:     "/lib/firmware/arm/mali/arch10.8/mali_csffw.bin",
// 		Typeflag: tar.TypeReg,
// 		Mode:     0o755,
// 		Size:     size,
// 	}); err != nil {
// 		return err
// 	}
// 	if _, err := io.Copy(t, firmwareFile); err != nil {
// 		return err
// 	}
// 	return t.Close()
// }

func main() {
	if os.Getenv("GOKRAZY_IN_DOCKER") == "1" {
		indockerMain()
	} else {
		if err := rebuildKernel(); err != nil {
			log.Fatal(err)
		}
	}
}
