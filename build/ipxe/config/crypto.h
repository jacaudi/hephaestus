/* hephaestus (design §5.10): no cross-signed certificate download (an empty value disables it), no OCSP. */
#undef CROSSCERT
#define CROSSCERT ""
#undef OCSP_CHECK
