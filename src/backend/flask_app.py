from flask import Flask
from werkzeug.middleware.proxy_fix import ProxyFix

import config
from .routes import pages, api, admin_api

app = Flask(__name__, template_folder='../frontend/templates', static_folder='../frontend/static')

# 生产 Web 端口仅允许受信反向代理访问时，按明确的代理跳数恢复真实客户端 IP。
if config.TRUST_PROXY_HOPS > 0:
    app.wsgi_app = ProxyFix(app.wsgi_app, x_for=config.TRUST_PROXY_HOPS)

app.register_blueprint(pages.bp) # load the blueprint for the all of the main web page routes
app.register_blueprint(api.bp, url_prefix='/api') # load the blueprint for the all of the api routes
app.register_blueprint(admin_api.bp, url_prefix='/api/admin') # load the blueprint for admin API routes

# Runs the main flask app
def run_flask_server(host, port):
    app.run(host=host, port=port, debug=False)
