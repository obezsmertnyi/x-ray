// Command devenv boots a local envtest control plane (kube-apiserver + etcd
// only — no kubelet, scheduler, or controllers), installs the MemoryCell CRD,
// writes a kubeconfig, and blocks until interrupted. X-Ray is then run as a
// separate process pointed at the written kubeconfig (KUBECONFIG=...), so its
// CellSyncer + MCP upserts persist MemoryCells into the envtest etcd.
//
// This is the ADR-038 local bootstrap. Usage:
//
//	go run ./hack/devenv                 # starts the control plane, prints KUBECONFIG
//	KUBECONFIG=.devenv/kubeconfig ./bin/xray --dev --namespace xray ...
//
// The control plane lives only as long as this process runs; Ctrl-C tears it
// down and removes its temp data.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	namespace = "xray"
	// Dev MCP token: matches --mcp-token-secret xray-mcp-token (Makefile
	// run-dev) and the hack/seed-xray-memory.sh / .mcp.json default. Created
	// here so MCP auth works the moment the control plane is up — envtest etcd
	// is ephemeral, so the Secret is (re)created on every devenv boot.
	mcpTokenSecret = "xray-mcp-token"
	mcpTokenValue  = "dev-admin-token-xray"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "devenv:", err)
		os.Exit(1)
	}
}

func run() error {
	crdDir, err := filepath.Abs("config/crd")
	if err != nil {
		return err
	}
	outDir, err := filepath.Abs(".devenv")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	kubeconfigPath := filepath.Join(outDir, "kubeconfig")

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}

	fmt.Println("devenv: starting envtest control plane (apiserver + etcd)…")
	cfg, err := env.Start()
	if err != nil {
		return fmt.Errorf("start envtest: %w (is setup-envtest's assets dir present? run: setup-envtest use)", err)
	}
	defer env.Stop() //nolint:errcheck

	if err := writeKubeconfig(kubeconfigPath, cfg.Host, cfg.CAData, cfg.CertData, cfg.KeyData); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}

	fmt.Println("devenv: control plane up.")
	fmt.Printf("devenv: API server: %s\n", cfg.Host)
	fmt.Printf("devenv: kubeconfig: %s\n", kubeconfigPath)
	fmt.Println("devenv: CRD installed: memorycells.xray.triageagent.dev")
	fmt.Println()
	fmt.Println("Run xray against it (auth disabled — connect as admin):")
	fmt.Printf("  KUBECONFIG=%s ./bin/xray --dev --namespace xray --mcp-address :8083 --mcp-oauth=false\n", kubeconfigPath)
	fmt.Println()
	fmt.Println("Inspect cells:")
	fmt.Printf("  KUBECONFIG=%s kubectl get mcell -n xray\n", kubeconfigPath)
	fmt.Println()
	fmt.Println("devenv: Ctrl-C to tear down.")

	// envtest does not create namespaces; xray writes MemoryCells into
	// --namespace, so create it up front (the apiserver won't auto-create it).
	if err := ensureNamespace(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "devenv: warning — could not pre-create namespace:", err)
	} else {
		fmt.Printf("devenv: namespace ready: %s\n", namespace)
	}

	// Create the MCP token Secret so the server's bearer auth works on boot.
	if err := ensureMCPSecret(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "devenv: warning — could not create MCP token secret:", err)
	} else {
		fmt.Printf("devenv: MCP token secret ready: %s (token=%s)\n", mcpTokenSecret, mcpTokenValue)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	fmt.Println("\ndevenv: shutting down control plane.")
	return nil
}

func writeKubeconfig(path, host string, ca, cert, key []byte) error {
	const ctx = "devenv"
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[ctx] = &clientcmdapi.Cluster{
		Server:                   host,
		CertificateAuthorityData: ca,
	}
	cfg.AuthInfos[ctx] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cert,
		ClientKeyData:         key,
	}
	cfg.Contexts[ctx] = &clientcmdapi.Context{
		Cluster:  ctx,
		AuthInfo: ctx,
	}
	cfg.CurrentContext = ctx
	return clientcmd.WriteToFile(*cfg, path)
}

func ensureNamespace(cfg *rest.Config) error {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	_, err = cs.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func ensureMCPSecret(cfg *rest.Config) error {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: mcpTokenSecret, Namespace: namespace},
		StringData: map[string]string{"token": mcpTokenValue},
	}
	_, err = cs.CoreV1().Secrets(namespace).Create(context.Background(), sec, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
