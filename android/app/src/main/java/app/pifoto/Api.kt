package app.pifoto

import android.net.Uri
import java.io.File
import java.io.IOException
import java.io.InputStream
import java.net.HttpURLConnection
import java.net.URL
import java.net.URLEncoder
import java.security.MessageDigest
import java.security.SecureRandom
import javax.crypto.Mac
import javax.crypto.spec.SecretKeySpec
import org.json.JSONObject

/**
 * Minimal client for the PiFoto server API (see main.go). Blocking: call off the main thread.
 * Calls that send the session only ever go to an address from [pickBase] (checked home or https).
 */
object Api {
    const val COOKIE = "pf_s"
    const val DEVICE_COOKIE = "pf_d"

    class AuthException : IOException("not logged in")
    /** 401 from /api/login: the name or the password is wrong (the server doesn't say which). */
    class WrongLoginException : IOException("Wrong name or password")
    /** The upload was aborted because the worker was stopped (not an error). */
    class StoppedException : IOException("stopped")
    /** 507: the person's storage limit on the Pi is reached. The file can go up once there's room. */
    class FullException(msg: String) : IOException(msg)

    /** A successful login: the session token and the person's display name ("" from servers before family accounts). */
    class Login(val session: String, val name: String, val device: String)

    /**
     * Logs [name] in (an empty name means the server's owner account, like before family accounts).
     * Returns the session, or throws with a readable message.
     */
    fun login(server: String, name: String, password: String, device: String = ""): Login {
        val c = open("$server/api/login", null)
        c.requestMethod = "POST"
        c.doOutput = true
        c.setRequestProperty("Content-Type", "application/json")
        // The device cookie (see Prefs.device) only goes over https: it is no secret worth much, but
        // plain HTTP is never proven to be the Pi before logging in.
        if (device.isNotEmpty() && server.startsWith("https://")) c.setRequestProperty("Cookie", "$DEVICE_COOKIE=$device")
        c.outputStream.use { it.write(JSONObject().put("name", name).put("password", password).toString().toByteArray()) }
        val code = c.responseCode
        if (code in 300..399) {
            val loc = c.getHeaderField("Location") ?: "?"
            c.disconnect()
            throw IOException("The server redirects to $loc — enter that address instead")
        }
        if (code != 204 && code != 200) {
            val msg = errorText(c).ifBlank { "HTTP $code" }
            c.disconnect()
            throw when (code) {
                401 -> WrongLoginException()
                429 -> IOException("Too many attempts. Wait a few minutes, then try again.")
                else -> IOException(msg)
            }
        }
        val cookies = c.headerFields.entries
            .filter { it.key.equals("Set-Cookie", ignoreCase = true) }
            .flatMap { it.value }
        fun cookie(name: String) = cookies.firstNotNullOfOrNull { h ->
            h.split(';').first().trim().takeIf { it.startsWith("$name=") }?.substringAfter('=')
        }
        val token = cookie(COOKIE)
        val dev = cookie(DEVICE_COOKIE).orEmpty()
        // 200 {"id","name","admin","pending"}; servers before family accounts answer 204 without a body.
        val who = if (code == 200) try {
            JSONObject(c.inputStream.bufferedReader().use { it.readText() }).optString("name", "")
        } catch (_: Exception) { "" } else ""
        c.disconnect()
        if (token.isNullOrEmpty()) throw IOException("Server sent no session")
        return Login(token, who, dev)
    }

    /** Who is logged in: GET /api/me. */
    class Me(val name: String, val admin: Boolean)

    /**
     * Who [session] belongs to. Throws AuthException if the server no longer knows the session
     * (logged out elsewhere, password reset, person removed), IOException if it can't be asked.
     * Returns null for servers before family accounts (no /api/me).
     */
    fun me(server: String, session: String): Me? {
        val c = open("$server/api/me", session)
        c.connectTimeout = 5000; c.readTimeout = 8000
        try {
            return when (c.responseCode) {
                200 -> {
                    val txt = c.inputStream.bufferedReader().use { it.readText() }
                    val json = try { JSONObject(txt) } catch (e: org.json.JSONException) {
                        throw IOException("unexpected reply (captive portal or proxy?)")
                    }
                    if (!json.has("name")) throw IOException("unexpected reply")
                    Me(json.optString("name", ""), json.optBoolean("admin"))
                }
                401 -> throw AuthException()
                404 -> null
                else -> throw IOException("HTTP ${c.responseCode}")
            }
        } finally { c.disconnect() }
    }

    /** Ends the session on the server (best effort). */
    fun logout(server: String, session: String) {
        try {
            val c = open("$server/api/logout", session)
            c.requestMethod = "POST"
            c.setRequestProperty("X-PiFoto", "1")
            c.responseCode
            c.disconnect()
        } catch (_: IOException) {}
    }

    /**
     * The server's address for use away from home ("" if not configured or not https). Only asked
     * over https: the answer becomes the address the app sends its session to without checking, so
     * it must come from a server whose certificate proves it is the Pi.
     */
    fun remoteAddress(server: String, session: String): String {
        if (!server.startsWith("https://")) return ""
        val c = open("$server/api/info", session)
        return try {
            if (c.responseCode != 200) "" else
                JSONObject(c.inputStream.bufferedReader().use { it.readText() }).optString("remote", "")
                    .takeIf { it.startsWith("https://") } ?: ""
        } catch (_: Exception) { "" } finally { c.disconnect() }
    }

    /** Does anything answer at [base]? (Any HTTP status counts.) Sends no session: only for display. */
    fun reachable(base: String): Boolean = try {
        val c = open("$base/api/status", null)
        c.connectTimeout = 2500; c.readTimeout = 4000
        val code = c.responseCode
        c.disconnect()
        code in 200..499
    } catch (_: Exception) { false }

    private val rng = SecureRandom()

    /**
     * Is [base] really our Pi, and does it know [session]? The server's "hello" check: the app sends
     * only a hash of the session and a fresh nonce (never the session itself, no cookie), and the Pi
     * proves it holds the session by answering HMAC-SHA256(session, "pifoto-hello:" + nonce).
     * Whatever else answers at the plain-HTTP home address on another network (a hotel Wi-Fi, a
     * fake DNS, a different "raspberrypi" on a friend's router) can't, so it never gets the session,
     * the password or photos. Short timeouts like [reachable]: it decides home vs. away and must not
     * hold up the page. Any error counts as no.
     */
    fun verifyHome(base: String, session: String): Boolean {
        if (session.isEmpty()) return false
        return try {
            val nonce = hex(ByteArray(16).also { rng.nextBytes(it) })
            val id = hex(MessageDigest.getInstance("SHA-256").digest(session.toByteArray(Charsets.UTF_8)))
            val c = open("$base/api/hello?s=$id&n=$nonce", null) // null: no Cookie header
            c.connectTimeout = 2500; c.readTimeout = 4000
            c.useCaches = false
            try {
                if (c.responseCode != 200) return false
                // Small and bounded: a fake server must not be able to make us read forever.
                val got = JSONObject(c.inputStream.use { readSmall(it) }).optString("mac", "").lowercase()
                val hmac = Mac.getInstance("HmacSHA256")
                hmac.init(SecretKeySpec(session.toByteArray(Charsets.UTF_8), "HmacSHA256"))
                val want = hex(hmac.doFinal("pifoto-hello:$nonce".toByteArray(Charsets.UTF_8)))
                MessageDigest.isEqual(want.toByteArray(Charsets.US_ASCII), got.toByteArray(Charsets.US_ASCII))
            } finally { c.disconnect() }
        } catch (_: Exception) { false }
    }

    /**
     * Where to talk to the Pi right now with [session]: the home address when it proves to be our
     * Pi (fast, direct, works even when the internet is down), otherwise the public https address,
     * whose certificate proves who it is. Never an unchecked plain-HTTP address. Logged out: the
     * public address. Blocking.
     */
    fun pickBase(p: Prefs, session: String = p.session): String =
        if (session.isNotEmpty() && verifyHome(p.server, session)) p.server else p.remote

    /** "scheme://host[:port]" of [url] for same-origin checks (lowercase, default port dropped); "" unless http(s). */
    fun origin(url: String?): String {
        val u = Uri.parse(url ?: return "")
        val scheme = u.scheme?.lowercase() ?: return ""
        if (scheme != "http" && scheme != "https") return ""
        val host = u.host?.lowercase()?.takeIf { it.isNotEmpty() } ?: return ""
        val port = u.port.takeIf { it != -1 && it != (if (scheme == "https") 443 else 80) }
        return "$scheme://$host" + if (port != null) ":$port" else ""
    }

    /**
     * How many more bytes [session]'s storage limit allows (GET /api/status: quota, used), or
     * Long.MAX_VALUE without a limit, on servers before limits, or when it can't be asked (the
     * server checks every upload anyway). [server] must come from [pickBase]: it gets the session.
     */
    fun room(server: String, session: String): Long = try {
        val c = open("$server/api/status", session)
        c.connectTimeout = 5000; c.readTimeout = 15000
        try {
            if (c.responseCode != 200) Long.MAX_VALUE else {
                val j = JSONObject(c.inputStream.bufferedReader().use { it.readText() })
                val quota = j.optLong("quota", 0)
                if (quota <= 0) Long.MAX_VALUE else (quota - j.optLong("used", 0)).coerceAtLeast(0)
            }
        } finally { c.disconnect() }
    } catch (_: Exception) { Long.MAX_VALUE }

    /** The app published on the Pi (GET /api/app; see appupdate.go). */
    class Release(val code: Long, val name: String, val notes: String, val size: Long, val sha256: String)

    /**
     * The published app, or null when none is (404, older servers). Tells the server which version
     * runs here, for the admin page. [server] must come from [pickBase]: it gets the session.
     */
    fun latestApp(server: String, session: String, installedCode: Long, installedName: String): Release? {
        val c = open("$server/api/app", session)
        c.connectTimeout = 8000; c.readTimeout = 20000
        val name = installedName.filter { it.isLetterOrDigit() || it in ".-_" }.take(20)
        c.setRequestProperty("X-PiFoto-App", "$installedCode $name".trim())
        try {
            return when (c.responseCode) {
                200 -> {
                    val j = JSONObject(c.inputStream.use { readSmall(it, 16384) })
                    Release(j.getLong("versionCode"), j.optString("versionName", ""), j.optString("notes", ""),
                        j.getLong("size"), j.getString("sha256").lowercase())
                }
                401 -> throw AuthException()
                else -> null
            }
        } catch (e: org.json.JSONException) {
            return null
        } finally { c.disconnect() }
    }

    /**
     * Downloads the published APK (GET /app/PiFoto.apk, public, no session) to [out] and checks its size and
     * SHA-256 against [rel]. Android itself refuses to install it unless it is signed with the same
     * key as this app, so a wrong file can't do harm; the check catches broken downloads.
     */
    fun downloadApp(server: String, rel: Release, out: File) {
        if (rel.size <= 0 || rel.size > 200L * 1024 * 1024) throw IOException("unexpected app size ${rel.size}")
        val c = open("$server/app/PiFoto.apk", null)
        c.readTimeout = 60_000
        try {
            if (c.responseCode != 200) throw IOException("HTTP ${c.responseCode}")
            val md = MessageDigest.getInstance("SHA-256")
            var n = 0L
            c.inputStream.use { input -> out.outputStream().use { o ->
                val buf = ByteArray(64 * 1024)
                while (true) {
                    val r = input.read(buf); if (r < 0) break
                    n += r
                    if (n > rel.size) throw IOException("the app download is longer than announced")
                    md.update(buf, 0, r); o.write(buf, 0, r)
                }
            } }
            if (n != rel.size || hex(md.digest()) != rel.sha256) throw IOException("the app download is damaged")
        } catch (e: IOException) {
            out.delete(); throw e
        } finally { c.disconnect() }
    }

    fun sha256(f: File): String {
        val md = MessageDigest.getInstance("SHA-256")
        f.inputStream().use { input -> val buf = ByteArray(64 * 1024); while (true) { val r = input.read(buf); if (r < 0) break; md.update(buf, 0, r) } }
        return hex(md.digest())
    }

    /** False only if the server says the session is gone (401). Throws IOException if the server is unreachable. */
    fun checkSession(server: String, session: String): Boolean {
        val c = open("$server/api/status", session)
        val code = c.responseCode
        c.disconnect()
        return code != 401
    }

    sealed class UploadResult {
        data object Stored : UploadResult()
        data object Duplicate : UploadResult()
        /** Server refused the file for good (e.g. unsupported type) — skip it. */
        data class Rejected(val reason: String) : UploadResult()
    }

    /**
     * Streams [body] to the server (chunked, so a MediaStore SIZE that's out of date can't break it).
     * Throws AuthException on 401, StoppedException if [stop] turns true, IOException on transient errors.
     */
    fun upload(server: String, session: String, name: String, mtimeMs: Long,
               body: InputStream, stop: () -> Boolean, onProgress: (Long) -> Unit): UploadResult {
        val q = "name=" + URLEncoder.encode(name, "UTF-8") + "&mtime=" + mtimeMs
        val c = open("$server/api/upload?$q", session)
        c.requestMethod = "POST"
        c.doOutput = true
        c.readTimeout = 120_000 // the Pi may need a moment to fsync large files
        c.setRequestProperty("X-PiFoto", "1")
        c.setRequestProperty("Content-Type", "application/octet-stream")
        c.setChunkedStreamingMode(256 * 1024)
        try {
            c.outputStream.use { out ->
                val buf = ByteArray(128 * 1024)
                var sent = 0L
                while (true) {
                    if (stop()) throw StoppedException()
                    val n = body.read(buf)
                    if (n < 0) break
                    out.write(buf, 0, n)
                    sent += n
                    onProgress(sent)
                }
            }
            val code = c.responseCode
            return when (code) {
                200 -> {
                    val txt = c.inputStream.bufferedReader().use { it.readText() }
                    val json = try { JSONObject(txt) } catch (e: org.json.JSONException) {
                        throw IOException("unexpected reply (captive portal or proxy?)") // not PiFoto talking
                    }
                    if (!json.has("row")) throw IOException("unexpected reply")
                    if (json.optBoolean("dup")) UploadResult.Duplicate else UploadResult.Stored
                }
                401 -> throw AuthException()
                507 -> throw FullException(errorText(c).ifBlank { "Your storage on PiFoto is full" })
                // Only "this file can never be stored" is permanent; everything else is retried.
                400, 413, 415 -> UploadResult.Rejected(errorText(c).ifBlank { "HTTP $code" })
                else -> throw IOException("HTTP $code ${errorText(c)}")
            }
        } finally {
            c.disconnect()
        }
    }

    /** [session] null/"" = no Cookie header at all (the app installs no java.net.CookieHandler, and the
     *  WebView's cookies are separate), so such a request carries nothing worth stealing. */
    private fun open(url: String, session: String?): HttpURLConnection {
        val c = URL(url).openConnection() as HttpURLConnection
        c.connectTimeout = 10_000
        c.readTimeout = 30_000
        c.instanceFollowRedirects = false
        if (!session.isNullOrEmpty()) c.setRequestProperty("Cookie", "$COOKIE=$session")
        return c
    }

    private fun hex(b: ByteArray): String = b.joinToString("") { "%02x".format(it) }

    private fun readSmall(input: InputStream, max: Int = 4096): String {
        val buf = ByteArray(max)
        var n = 0
        while (n < max) { val r = input.read(buf, n, max - n); if (r < 0) break; n += r }
        return String(buf, 0, n, Charsets.UTF_8)
    }

    private fun errorText(c: HttpURLConnection): String =
        try { c.errorStream?.bufferedReader()?.use { it.readText().trim().take(200) } ?: "" } catch (_: IOException) { "" }
}
