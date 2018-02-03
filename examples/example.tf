data "template_file" "my-configmap" {
  template = "${file("manifests/my-configmap.yaml")}"

  vars {
    greeting = "${var.greeting}"
  }
}

resource "k8s_manifest" "my-configmap" {
  content = "${data.template_file.my-configmap.rendered}"
}

data "template_file" "nginx-deployment" {
  template = "${file("manifests/nginx-deployment.yaml")}"

  vars {
    replicas = "${var.replicas}"
  }
}

resource "k8s_manifest" "nginx-deployment" {
  content = "${data.template_file.nginx-deployment.rendered}"
}
