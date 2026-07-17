package checkpointsyncer

import "testing"

func TestObjectKey(t *testing.T) {
	got := ObjectKey("default", "web", "app",
		"/var/lib/kubelet/checkpoints/checkpoint-web_default-app-2026.tar")
	want := "default/web/app/checkpoint-web_default-app-2026.tar"
	if got != want {
		t.Fatalf("ObjectKey = %q, want %q", got, want)
	}
}

func TestExternalURIAndParse(t *testing.T) {
	uri := ExternalURI("my-bucket", "default/web/app/x.tar")
	if uri != "s3://my-bucket/default/web/app/x.tar" {
		t.Fatalf("ExternalURI = %q", uri)
	}
	bucket, key, err := ParseS3URI(uri)
	if err != nil || bucket != "my-bucket" || key != "default/web/app/x.tar" {
		t.Fatalf("ParseS3URI = %q,%q,%v", bucket, key, err)
	}
	if _, _, err := ParseS3URI("http://nope"); err == nil {
		t.Fatal("expected error for non-s3 URI")
	}
}
