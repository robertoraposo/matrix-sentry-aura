# Provider Session Broker v1

## Objetivo

El usuario inicia sesión una vez desde Matrix Admin y las capacidades
autorizadas quedan disponibles para AURA y otros clientes mediante MCP.

AURA nunca recibe contraseñas, cookies, refresh tokens ni credenciales
del proveedor.

## Principios

- Matrix es el plano de control y autorización.
- AURA es un cliente autorizado de Matrix.
- Solo se admiten clientes y flujos oficiales del proveedor.
- No se extraen sesiones de navegadores.
- No se usan endpoints privados o no documentados.
- Las sesiones están aisladas por tenant.
- Ningún secreto aparece en MCP, logs, journal o respuestas administrativas.

## Arquitectura

### sentrymcp

Expone herramientas MCP y aplica permisos por tenant:

- provider_list
- provider_status
- provider_invoke

No ejecuta directamente CLIs de terceros.

### sentryproviderd

Servicio interno administrado por Matrix que:

- ejecuta adaptadores oficiales;
- mantiene un perfil aislado por tenant y proveedor;
- inicia y revoca sesiones;
- limita tiempo, memoria y tamaño de salida;
- nunca se expone públicamente.

### sentryadmin

Añade una sección Proveedores con:

- Conectar;
- Desconectar;
- Estado;
- Cuenta;
- Capacidades;
- Último uso.

### AURA

Consulta provider_list y provider_status por MCP.
Solicita inferencias mediante provider_invoke.
Nunca lee ni almacena credenciales del proveedor.

## Proveedores iniciales

1. Ollama local
2. Codex CLI con inicio de sesión ChatGPT
3. Claude Code con inicio de sesión Claude
4. Otros proveedores solo mediante autenticación oficial compatible

## Seguridad

- Directorios separados por tenant y proveedor.
- Permisos mínimos del sistema de archivos.
- Red interna de Docker únicamente.
- Lista explícita de ejecutables permitidos.
- Timeout y límite de salida por ejecución.
- Auditoría de proveedor, tenant, modelo, duración y resultado.
- Nunca registrar prompts completos por defecto.
- Nunca devolver secretos a AURA.

## Entrega incremental

### Fase 1

- paquete providerbroker;
- registro y estado de proveedores;
- adaptador Ollama;
- herramientas provider_list y provider_status;
- pruebas unitarias.

### Fase 2

- servicio sentryproviderd;
- adaptadores Codex CLI y Claude Code;
- perfiles aislados;
- conexión y desconexión.

### Fase 3

- interfaz Proveedores en Matrix Admin;
- flujo de login desde el navegador;
- auditoría y revocación.

### Fase 4

- provider_invoke;
- integración de AURA;
- selección automática y failover.
