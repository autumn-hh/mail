<h1>
    <img src="pictures/icon.svg" height="32" width="auto" alt="logo" style="vertical-align: middle; margin-right: 15px;">
    Maildrop
</h1>

Maildrop is a self hostable and easy to use disposable email service that allows you to receive emails on a random email address on your domain.

The backend now runs in **Go**: HTTP APIs, SMTP reception, MIME parsing, SQLite storage, and scheduled cleanup run in one binary with the existing frontend embedded. The homepage includes an **管理员登录** link. Manage receiving domains in **管理员后台 → 系统设置 → 邮箱域名设置**; changes persist in SQLite and take effect immediately.

See [Go deployment and migration](docs/go-migration.md) for database compatibility, JSON import, rollback, and verification. Python files remain as legacy reference and regression fixtures; the Go service and Docker image do not run Python.

**Now with V2 Features:**
*   **Database Storage:** Robust SQLite support for better performance and data integrity.
*   **Credential Access:** Mailbox key exchange followed by Bearer-token API access, with a separate admin password.
*   **Enhanced API:** Comprehensive RESTful API V2 for integration and management.
*   **Flexible Retention:** Support for custom email retention periods (up to permanent storage).

![App Screenshot](pictures/index.png)

## App Features

### Use cases

- Signing up to services without using your real email address
- Easily creating multiple accounts on services
- System integration via REST API
- Automated email testing

### Features

- [x] **Random email generation**
- [x] **Use custom emails**
- [x] **Mailbox-key protected inboxes with Bearer API access**
- [x] **SQLite Database Support** (New in V2)
- [x] **Multi-domain support**
- [x] **Clean UI**
- [x] **Easy setup**
- [x] **RESTful API V2**
- [x] **Admin Panel** for managing mailboxes and system settings
- [x] **Configurable Retention** (from minutes to permanent)

### Planned Features

- [x] Better mobile UI
- [x] Settings web page (Admin Panel)
- [x] Multi domain support

## Running the application

### Prerequisites

- Go 1.26 or newer (or Docker Compose)

### Installation and running

1.  **Clone the repository**

    ```bash
    git clone https://github.com/autumuhh/mail.git
    cd mail
    ```

2.  **Configure the service**

    ```bash
    cp .env.example .env
    ```

    Set a strong administrator password in .env. Domains can be configured through the admin page after startup.

3.  **Build the Go binary**

    ```bash
    go build -o maildrop ./cmd/maildrop
    ```

4.  **Run the application**

    ```bash
    ./maildrop
    ```

On Linux, port 25 requires appropriate privileges or a container. For local development, set SMTP_PORT=2525 in .env.

### Connecting to your domain

1. Ensure port 25 is open as this is the port the email server uses. Some ISPs block this so you may need to use a tunnel or host maildrop in the cloud.
2. Edit your domains dns settings and create an `A` record pointing to your public IP.
3. Edit your domains dns settings and create an `MX` record pointing to the domain you made your `A` record on.
4. Open the homepage, choose **管理员登录**, and save your domains in **系统设置 → 邮箱域名设置**. DNS records must still be configured at your DNS provider.

### Running with docker

Build the local Go image with `docker build -t maildrop-go:local .`, then run:
```
sudo docker run \
  -d \
  --restart unless-stopped \
  --name maildrop \
  -p 5000:5000 \
  -p 25:25 \
  -e DOMAIN="yourdomain.com" \
  -e PASSWORD="replace-with-a-strong-admin-password" \
  -v ./data:/app/data \
  maildrop-go:local
```
Or if you prefer docker compose, Add this to your compose.yml file:
```
services:
  maildrop:
    build: .
    container_name: maildrop
    restart: unless-stopped
    ports:
      - "5000:5000"
      - "25:25"
    environment:
      - DOMAIN=yourdomain.com
      - USE_DATABASE=true
      - PASSWORD=replace-with-a-strong-admin-password
```
and then start it: `docker compose up -d --build`. The repository's compose file publishes the web UI on port 8081 and persists `./data`.

Set a strong `PASSWORD` in `.env` or the container environment before using `/admin`.
If it is omitted, the management API intentionally stays locked instead of accepting a default password.

## Documentation

*   [Deployment and Usage Guide](DEPLOYMENT_GUIDE.md) - **Recommended for beginners**
*   [API Documentation (V2)](API_DOCUMENTATION.md)
