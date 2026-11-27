"""验证真实邮箱预览的样式、点击链接和自动加载 HTTPS 图片，仅访问回环地址。"""

import base64

from playwright.sync_api import expect, sync_playwright

from verify_ui_browser import isolated_site, mailbox_path


LOGO = "https://assets.example.invalid/logo.png"
SIGN_IN = "https://login.example.invalid/sign-in"
PNG = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
)
MAIL = f"""<!doctype html><html><head><style>.action {{ color: rgb(253, 254, 255) }}</style></head>
<body><img alt="Logo" src="{LOGO}">
<a class="action" style="background-color: #123456" href="{SIGN_IN}">Sign in</a>
<a href="data:text/plain,fixture">Unsupported link</a></body></html>"""


def run():
    with isolated_site() as (base, fixtures, add_message), sync_playwright() as playwright:
        first = add_message("Preview fixture A", MAIL, html=True)
        second = add_message("Preview fixture B", MAIL, html=True)
        browser = playwright.chromium.launch()
        context = browser.new_context()
        external = []

        def intercept(route):
            request = route.request
            if request.url.startswith(base + "/"):
                route.continue_()
                return
            external.append((request.url, request.headers.get("referer")))
            if request.url == LOGO:
                route.fulfill(content_type="image/png", body=PNG)
            elif request.url == SIGN_IN:
                route.fulfill(content_type="text/html", body="<title>Test destination</title>")
            else:
                route.abort()

        context.route("**/*", intercept)
        page = context.new_page()
        page.goto(base + mailbox_path(fixtures["boxes"]["studio"]))
        frame = page.frame_locator("#detail-body iframe").frame_locator("#message")
        action = frame.get_by_role("link", name="Sign in")

        page.locator(f'.email-row[data-email-id="{first}"] .email-open').click()
        expect(action).to_have_css("color", "rgb(253, 254, 255)")
        expect(action).to_have_css("background-color", "rgb(18, 52, 86)")
        assert frame.get_by_text("Unsupported link").get_attribute("href") is None
        expect(frame.locator("img")).to_have_js_property("naturalWidth", 1)
        assert external == [(LOGO, None)], external

        with context.expect_page() as opened:
            action.click()
        popup = opened.value
        expect(popup).to_have_title("Test destination")
        assert popup.evaluate("window.opener === null && document.referrer === ''")
        popup.close()
        assert external == [(LOGO, None), (SIGN_IN, None)], external

        page.locator("#detail-back").click()
        expect(page.locator("#inbox-view")).to_be_visible()
        page.locator(f'.email-row[data-email-id="{second}"] .email-open').click()
        expect(action).to_be_visible()
        expect(frame.locator("img")).to_have_js_property("naturalWidth", 1)
        assert external == [(LOGO, None), (SIGN_IN, None), (LOGO, None)], external
        browser.close()
    print("PASS: message CSS, automatic HTTPS images, click-only links, no referrer/opener")


if __name__ == "__main__":
    run()
