machine:
  certSANs:
    - {{ .Node.Endpoint }}
  nodeLabels:
    hephaestus.example.test/zone: {{ .Data.zone }}
    hephaestus.example.test/join: {{ .Data.joinToken }}
