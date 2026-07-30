package sshclient

import (
	"fmt"
	"path/filepath"
)

var fixedSSHOptions = []string{
	"-o", "ControlPersist=no",
	"-o", "ForwardAgent=no",
	"-o", "ForwardX11=no",
	"-o", "ForwardX11Trusted=no",
	"-o", "ClearAllForwardings=yes",
	"-o", "PermitLocalCommand=no",
	"-o", "SendEnv=-*",
}

var fixedUploadSSHOptions = []string{
	"-o", "BatchMode=yes",
	"-o", "NumberOfPasswordPrompts=0",
	"-o", "PubkeyAuthentication=no",
	"-o", "PasswordAuthentication=no",
	"-o", "KbdInteractiveAuthentication=no",
	"-o", "ChallengeResponseAuthentication=no",
	"-o", "GSSAPIAuthentication=no",
	"-o", "HostbasedAuthentication=no",
}

func primaryArguments(request Request, controlPath string) ([]string, error) {
	workspace, version, err := remoteTokens(request, controlPath)
	if err != nil {
		return nil, err
	}

	arguments := []string{"-M", "-S", controlPath, "-o", "ControlMaster=yes"}
	arguments = append(arguments, fixedSSHOptions...)
	arguments = append(arguments,
		"-tt",
		request.Destination,
		"pips",
		"__bridge-session",
		"--nonce", request.Nonce,
		"--version-token", version,
		"--workspace-token", workspace,
	)

	return arguments, nil
}

func uploadArguments(request Request, controlPath string) ([]string, error) {
	_, version, err := remoteTokens(request, controlPath)
	if err != nil {
		return nil, err
	}

	arguments := []string{"-S", controlPath, "-o", "ControlMaster=no"}
	arguments = append(arguments, fixedSSHOptions...)
	arguments = append(arguments, fixedUploadSSHOptions...)
	arguments = append(arguments,
		"-T",
		request.Destination,
		"pips",
		"__bridge-upload",
		"--nonce", request.Nonce,
		"--version-token", version,
	)

	return arguments, nil
}

func remoteTokens(request Request, controlPath string) (string, string, error) {
	if err := ValidateRequest(request); err != nil {
		return "", "", err
	}

	if !filepath.IsAbs(controlPath) || filepath.Base(controlPath) != "master.sock" {
		return "", "", fmt.Errorf("%w: invalid ControlPath", ErrInvalid)
	}

	workspace, err := EncodeWorkspace(request.Workspace)
	if err != nil {
		return "", "", err
	}

	version, err := EncodeVersion(request.Version)
	if err != nil {
		return "", "", err
	}

	return workspace, version, nil
}
