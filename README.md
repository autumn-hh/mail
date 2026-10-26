<h1>
    <img src="pictures/icon.svg" height="32" width="auto" alt="logo" style="vertical-align: middle; margin-right: 15px;">
    Maildrop
</h1>

Maildrop is a self hostable and easy to use disposable email service that allows you to receive emails on a random email address on your domain.

**Now with V2 Features:**
*   **Single Go Binary:** Web UI, REST API and SMTP server in one self-contained executable.
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
- [x] **Admin Panel** for managing mailboxes, domains and system settings (admin login also available from the homepage)
- [x] **Configurable Retention** (from minutes to permanent)

### Planned Features

- [x] Better mobile UI
- [x] Settings web page (Admin Panel)
- [x] Multi domain support

## Running the application

Maildrop is a single Go binary that runs the web UI/API, the SMTP server and the cleanup job.
The frontend is embedded into the binary, and data is stored in SQLite (`data/mailbox.db`).

### Prerequisites

- Go 1.26+ (only needed to build from source)

### Installation and running

1.  **Clone the repository**

    ```bash
    git clone https://github.com/autumuhh/mail.git
    cd mail
    ```

2.  **Configure**

    ```bash
    cp .env.example .env
    # edit .env: set DOMAINS and a strong PASSWORD
    ```

3.  **Build and run**

    ```bash
    go build -o maildrop ./cmd/maildrop
    sudo ./maildrop
    ```

**Binding the SMTP port 25 requires root (or `CAP_NET_BIND_SERVICE`).**

### Managing domains

`DOMAINS` in `.env` only seeds the domain list on the very first start.
After that, add, enable, disable or remove receiving domains in the admin panel (`/admin` → 域名设置);
changes take effect immediately without a restart.

### Upgrading from the Python version

Stop the old service, back up `data/mailbox.db`, then start the Go binary with the same `.env` and `data/` directory.
The database schema is migrated automatically on first start. JSON storage (`USE_DATABASE=false`) is no longer supported,
and `FLASK_SECRET_KEY` is no longer used.

### Connecting to your domain

1. Ensure port 25 is open as this is the port the email server uses. Some ISPs block this so you may need to use a tunnel or host maildrop in the cloud.
2. Edit your domains dns settings and create an `A` record pointing to your public IP.
3. Edit your domains dns settings and create an `MX` record pointing to the domain you made your `A` record on.
4. Set `DOMAINS` in `.env` before the first start, or add the domain later in the admin panel (域名设置).

### Running with docker

Use this command to run maildrop in a docker container:
```
sudo docker run \
  -d \
  --restart unless-stopped \
  --name maildrop \
  -p 5000:5000 \
  -p 25:25 \
  -e DOMAIN="yourdomain.com" \
  -e PASSWORD="replace-with-a-strong-admin-password" \
  autumuhh/mail:latest
```
Or if you prefer docker compose, Add this to your compose.yml file:
```
services:
  maildrop:
    image: autumuhh/mail:latest
    container_name: maildrop
    restart: unless-stopped
    ports:
      - "5000:5000"
      - "25:25"
    volumes:
      - ./data:/app/data
    environment:
      - DOMAINS=yourdomain.com
      - PASSWORD=replace-with-a-strong-admin-password
```
and then start it: `sudo docker compose up -d`

Set a strong `PASSWORD` in `.env` or the container environment before using `/admin`.
If it is omitted, the management API intentionally stays locked instead of accepting a default password.

## Documentation

*   [Deployment and Usage Guide](DEPLOYMENT_GUIDE.md) - **Recommended for beginners**
*   [API Documentation (V2)](API_DOCUMENTATION.md)
