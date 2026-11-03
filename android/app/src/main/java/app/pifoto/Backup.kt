package app.pifoto

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.ContentResolver
import android.content.ContentUris
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.content.pm.ServiceInfo
import android.net.ConnectivityManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.provider.MediaStore
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.ExistingWorkPolicy
import androidx.work.ForegroundInfo
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.OutOfQuotaPolicy
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import java.io.FileNotFoundException
import java.io.IOException
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import kotlin.coroutines.cancellation.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

/** Scheduling + MediaStore helpers for automatic backup. */
object Backup {
    private const val PERIODIC = "pifoto-periodic"
    private const val TRIGGER = "pifoto-trigger"
    private const val NOW = "pifoto-now"
    private const val RETRY = "pifoto-retry"
    const val TAG_TRIGGER = "trigger"
    const val CHANNEL = "backup"
    const val NOTIF_ID = 1
    const val NOTIF_AUTH_ID = 2
    private const val OWN_DOWNLOADS = "Pictures/PiFoto/" // files the app downloaded from the server

    /** Files this big are left for a run with a foreground service (background runs get ~10 minutes). */
    const val BIG_FILE = 500L * 1024 * 1024

    /** Only one backup pass may run at a time (periodic, trigger and manual runs can overlap). */
    val running = AtomicBoolean(false)

    fun hasMediaPermission(ctx: Context): Boolean {
        val perms = if (Build.VERSION.SDK_INT >= 33)
            listOf(Manifest.permission.READ_MEDIA_IMAGES, Manifest.permission.READ_MEDIA_VIDEO)
        else listOf(Manifest.permission.READ_EXTERNAL_STORAGE)
        return perms.all { ctx.checkSelfPermission(it) == PackageManager.PERMISSION_GRANTED }
    }

    private fun constraints(p: Prefs, manual: Boolean = false) = Constraints.Builder()
        .setRequiredNetworkType(if (p.wifiOnly) NetworkType.UNMETERED else NetworkType.CONNECTED)
        .setRequiresCharging(p.chargingOnly && !manual)
        .build()

    /** (Re)apply the schedule after any settings change. */
    fun schedule(ctx: Context) {
        val p = Prefs(ctx)
        val wm = WorkManager.getInstance(ctx)
        if (!p.backupEnabled || !p.loggedIn) {
            for (n in listOf(PERIODIC, TRIGGER, NOW, RETRY)) wm.cancelUniqueWork(n)
            return
        }
        wm.enqueueUniquePeriodicWork(PERIODIC, ExistingPeriodicWorkPolicy.UPDATE,
            PeriodicWorkRequestBuilder<BackupWorker>(1, TimeUnit.HOURS).setConstraints(constraints(p)).build())
        wm.enqueueUniqueWork(TRIGGER, ExistingWorkPolicy.REPLACE, triggerRequest(p))
    }

    /** Content-trigger jobs are single-use: the trigger worker re-arms the next one when it finishes.
     *  APPEND_OR_REPLACE keeps exactly one waiting (and starts fresh if the old chain was cancelled). */
    fun rearmTrigger(ctx: Context) {
        val p = Prefs(ctx)
        if (!p.backupEnabled || !p.loggedIn) return
        WorkManager.getInstance(ctx).enqueueUniqueWork(TRIGGER, ExistingWorkPolicy.APPEND_OR_REPLACE, triggerRequest(p))
    }

    private fun triggerRequest(p: Prefs) = OneTimeWorkRequestBuilder<BackupWorker>()
        .setConstraints(Constraints.Builder()
            .setRequiredNetworkType(if (p.wifiOnly) NetworkType.UNMETERED else NetworkType.CONNECTED)
            .setRequiresCharging(p.chargingOnly)
            .addContentUriTrigger(MediaStore.Images.Media.EXTERNAL_CONTENT_URI, true)
            .addContentUriTrigger(MediaStore.Video.Media.EXTERNAL_CONTENT_URI, true)
            .setTriggerContentUpdateDelay(15, TimeUnit.SECONDS)
            .setTriggerContentMaxDelay(2, TimeUnit.MINUTES)
            .build())
        .addTag(TAG_TRIGGER)
        .build()

    /** Transient failure (Pi offline, Wi-Fi gone…): try again in a while, independent of the trigger chain. */
    fun scheduleRetry(ctx: Context) {
        val p = Prefs(ctx)
        if (!p.backupEnabled || !p.loggedIn) return
        WorkManager.getInstance(ctx).enqueueUniqueWork(RETRY, ExistingWorkPolicy.KEEP,
            OneTimeWorkRequestBuilder<BackupWorker>()
                .setConstraints(constraints(p))
                .setInitialDelay(15, TimeUnit.MINUTES)
                .build())
    }

    /** Manual / app-opened run. Started while the app is visible, so it may use a foreground service. */
    fun runNow(ctx: Context) {
        val p = Prefs(ctx)
        if (!p.backupEnabled || !p.loggedIn) return
        WorkManager.getInstance(ctx).enqueueUniqueWork(NOW, ExistingWorkPolicy.KEEP,
            OneTimeWorkRequestBuilder<BackupWorker>()
                .setConstraints(constraints(p, manual = true))
                .setExpedited(OutOfQuotaPolicy.RUN_AS_NON_EXPEDITED_WORK_REQUEST)
                .build())
    }

    // ---------- MediaStore ----------

    /** [skip] = advance past it without uploading (own download / abandoned). */
    data class Media(val id: Long, val uri: Uri, val name: String, val size: Long, val takenMs: Long,
                     val generation: Long, val pending: Boolean, val skip: Boolean)

    /** External volumes (internal storage + SD cards). Each has its own generation counter. */
    fun volumes(ctx: Context): Set<String> = MediaStore.getExternalVolumeNames(ctx)

    /** Marks everything currently on the device as already handled ("only back up new photos"). */
    fun skipExisting(ctx: Context) {
        val p = Prefs(ctx)
        for (v in volumes(ctx)) {
            p.setWatermark(v, MediaStore.getGeneration(ctx, v)); p.setDoneAbove(v, emptySet())
            p.setMediaVersion(v, MediaStore.getVersion(ctx, v))
        }
    }

    fun backupAllExisting(ctx: Context) {
        val p = Prefs(ctx)
        for (v in volumes(ctx)) {
            p.setWatermark(v, 0); p.setDoneAbove(v, emptySet())
            p.setMediaVersion(v, MediaStore.getVersion(ctx, v))
        }
    }

    /**
     * Generations are only comparable while MediaStore.getVersion stays the same. After a media
     * database rebuild (OS update, cleared Media Storage, reformatted SD card) the old watermark would
     * hide every file, so re-anchor it: everything added since the last check (minus a day) is redone;
     * the server skips files it already has.
     */
    fun checkVersions(ctx: Context) {
        val p = Prefs(ctx)
        for (v in volumes(ctx)) {
            val now = MediaStore.getVersion(ctx, v)
            val old = p.mediaVersion(v)
            if (old == now) continue
            if (old != null) {
                val cutoff = (if (p.lastRun > 0) p.lastRun / 1000 else 0) - 86_400
                var minGen = Long.MAX_VALUE
                ctx.contentResolver.query(MediaStore.Files.getContentUri(v),
                    arrayOf(MediaStore.MediaColumns.GENERATION_ADDED),
                    "${MediaStore.MediaColumns.DATE_ADDED}>?", arrayOf(cutoff.toString()), null)?.use { c ->
                    while (c.moveToNext()) minGen = minOf(minGen, c.getLong(0))
                }
                p.setWatermark(v, if (minGen == Long.MAX_VALUE) MediaStore.getGeneration(ctx, v) else minGen - 1)
                p.setDoneAbove(v, emptySet())
            } // old == null: a new volume (e.g. SD card inserted later) — watermark 0 backs it up
            p.setMediaVersion(v, now)
        }
    }

    /** Photos and videos on [volume] added after [after], oldest first, including files still being written. */
    fun pendingOn(ctx: Context, volume: String, after: Long): List<Media> {
        val out = ArrayList<Media>()
        val proj = arrayOf(
            MediaStore.Files.FileColumns._ID, MediaStore.Files.FileColumns.MEDIA_TYPE,
            MediaStore.MediaColumns.DISPLAY_NAME, MediaStore.MediaColumns.SIZE,
            MediaStore.MediaColumns.DATE_TAKEN, MediaStore.MediaColumns.DATE_MODIFIED,
            MediaStore.MediaColumns.GENERATION_ADDED, MediaStore.MediaColumns.IS_PENDING,
            MediaStore.MediaColumns.RELATIVE_PATH, MediaStore.MediaColumns.DATE_EXPIRES,
        )
        val args = Bundle().apply {
            putString(ContentResolver.QUERY_ARG_SQL_SELECTION,
                "(${MediaStore.Files.FileColumns.MEDIA_TYPE}=? OR ${MediaStore.Files.FileColumns.MEDIA_TYPE}=?) " +
                    "AND ${MediaStore.MediaColumns.GENERATION_ADDED}>?")
            putStringArray(ContentResolver.QUERY_ARG_SQL_SELECTION_ARGS, arrayOf(
                MediaStore.Files.FileColumns.MEDIA_TYPE_IMAGE.toString(),
                MediaStore.Files.FileColumns.MEDIA_TYPE_VIDEO.toString(), after.toString()))
            putString(ContentResolver.QUERY_ARG_SQL_SORT_ORDER, "${MediaStore.MediaColumns.GENERATION_ADDED} ASC")
            putInt(MediaStore.QUERY_ARG_MATCH_PENDING, MediaStore.MATCH_INCLUDE)
        }
        val nowSec = System.currentTimeMillis() / 1000
        ctx.contentResolver.query(MediaStore.Files.getContentUri(volume), proj, args, null)?.use { c ->
            while (c.moveToNext()) {
                val id = c.getLong(0)
                val video = c.getInt(1) == MediaStore.Files.FileColumns.MEDIA_TYPE_VIDEO
                val base = if (video) MediaStore.Video.Media.getContentUri(volume) else MediaStore.Images.Media.getContentUri(volume)
                val taken = c.getLong(4).takeIf { it > 0 } ?: (c.getLong(5) * 1000)
                val path = c.getString(8) ?: ""
                val isPending = c.getInt(7) != 0
                // Pending past its expiry = abandoned by its app (MediaStore will delete it).
                val abandoned = isPending && !c.isNull(9) && c.getLong(9) in 1 until nowSec
                out += Media(id, ContentUris.withAppendedId(base, id), c.getString(2) ?: "file", c.getLong(3),
                    taken, c.getLong(6), pending = isPending && !abandoned,
                    skip = abandoned || path.startsWith(OWN_DOWNLOADS))
            }
        }
        return out
    }

    fun exists(ctx: Context, uri: Uri): Boolean =
        ctx.contentResolver.query(uri, arrayOf(MediaStore.MediaColumns._ID), null, null, null)?.use { it.count > 0 } ?: false

    // ---------- notifications ----------

    fun ensureChannel(ctx: Context) {
        val nm = ctx.getSystemService(NotificationManager::class.java)
        if (nm.getNotificationChannel(CHANNEL) == null) {
            nm.createNotificationChannel(NotificationChannel(CHANNEL, "Photo backup", NotificationManager.IMPORTANCE_LOW))
        }
    }

    fun openSettingsIntent(ctx: Context): PendingIntent = PendingIntent.getActivity(ctx, 0,
        Intent(ctx, SettingsActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
        PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)

    fun progressNotification(ctx: Context, text: String, done: Int, total: Int): Notification {
        ensureChannel(ctx)
        return Notification.Builder(ctx, CHANNEL)
            .setSmallIcon(R.drawable.ic_notif)
            .setContentTitle("Backing up to PiFoto")
            .setContentText(text)
            .setProgress(total, done, total == 0)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setContentIntent(openSettingsIntent(ctx))
            .build()
    }

    fun canNotify(ctx: Context) = Build.VERSION.SDK_INT < 33 ||
        ctx.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED
}

class BackupWorker(ctx: Context, params: WorkerParameters) : CoroutineWorker(ctx, params) {

    /** Retry later; [code] is the BackupStatus.PROBLEM_* shown while waiting. */
    private class Transient(msg: String, val code: String) : Exception(msg)

    override suspend fun getForegroundInfo(): ForegroundInfo = foreground("Checking for new photos…", 0, 0)

    private fun foreground(text: String, done: Int, total: Int) = ForegroundInfo(
        Backup.NOTIF_ID, Backup.progressNotification(applicationContext, text, done, total),
        ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)

    /** Never returns failure: a failed trigger work would also fail the re-armed one chained after it. */
    override suspend fun doWork(): Result {
        val ctx = applicationContext
        val p = Prefs(ctx)
        try {
            if (!p.backupEnabled || !p.loggedIn) return Result.success()
            if (!Backup.hasMediaPermission(ctx)) {
                p.lastStatus = "Paused: photo access permission is missing"
                return Result.success()
            }
            if (!Backup.running.compareAndSet(false, true)) return Result.success() // the active pass re-checks at its end
            BackupStatus.start(System.currentTimeMillis())
            try {
                withContext(Dispatchers.IO) { runPass(ctx, p) }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Transient) {
                p.lastStatus = e.message ?: "Will retry"
                p.problem = e.code
                Backup.scheduleRetry(ctx)
            } catch (e: Exception) {
                p.lastStatus = "Error: ${e.javaClass.simpleName} ${e.message?.take(80) ?: ""}. Will retry."
                p.problem = BackupStatus.PROBLEM_ERROR
                Backup.scheduleRetry(ctx)
            } finally {
                BackupStatus.clear()
                Backup.running.set(false)
                ctx.getSystemService(NotificationManager::class.java).cancel(Backup.NOTIF_ID)
            }
            return Result.success()
        } finally {
            if (tags.contains(Backup.TAG_TRIGGER) && !isStopped) Backup.rearmTrigger(ctx)
        }
    }

    private suspend fun runPass(ctx: Context, p: Prefs) {
        Backup.checkVersions(ctx)
        val cm = ctx.getSystemService(ConnectivityManager::class.java)
        var session = p.session
        var net = cm.activeNetwork // before the check: a switch during it is caught below
        var base = Api.pickBase(p, session) // the checked home address, or the public https one
        // Storage limit: a file that can't fit isn't sent at all (the Pi would only refuse it after
        // the whole upload). Counted down as files are stored; the server has the last word.
        var room = Api.room(base, session)
        p.lastRun = System.currentTimeMillis()
        var fg = false
        var stored = 0; var dups = 0; var skipped = 0; var done = 0
        var bytesDone = 0L // MediaStore sizes of the files finished in this pass (for the progress bar)
        var bigWaiting = false
        var full = false // something didn't fit the storage limit: it waits, smaller files after it still go
        var lastNotify = 0L

        // Re-query after each round so photos taken during a long pass are picked up too.
        for (round in 0 until 5) {
            var uploadedThisRound = 0
            val pending = Backup.volumes(ctx).map { v -> v to Backup.pendingOn(ctx, v, p.watermark(v)) }
            // Live progress for the settings screen and the web app: everything this round will try to upload.
            val todo = pending.flatMap { (v, items) ->
                val ds = p.doneAbove(v); items.filter { !it.pending && !it.skip && it.id !in ds }
            }
            BackupStatus.update { it.copy(total = done + todo.size, done = done, bytesDone = bytesDone,
                bytesTotal = bytesDone + todo.sumOf { m -> m.size.coerceAtLeast(0) }) }
            for ((volume, items) in pending) {
                if (items.isEmpty()) { p.setDoneAbove(volume, emptySet()); continue }
                val doneSet = p.doneAbove(volume).toMutableSet()
                var blocked = false // a pending/deferred item: the watermark must not move past it
                val total = items.count { !it.pending && !it.skip && it.id !in doneSet }

                fun advance(m: Backup.Media) {
                    if (!blocked) p.setWatermark(volume, m.generation)
                    else { doneSet += m.id; p.setDoneAbove(volume, doneSet) }
                }

                for (m in items) {
                    if (isStopped) return
                    if (m.pending) { blocked = true; continue } // still being written; fine once finished
                    if (m.skip || m.id in doneSet) { advance(m); continue }
                    if (!fg && total > 0) {
                        // Android may refuse a foreground service when started from the background.
                        fg = try { setForeground(foreground("Preparing…", 0, total)); true } catch (_: Exception) { false }
                    }
                    if (!fg && m.size > Backup.BIG_FILE) {
                        bigWaiting = true; blocked = true
                        BackupStatus.update { it.copy(total = it.total - 1, bytesTotal = it.bytesTotal - m.size.coerceAtLeast(0)) }
                        continue
                    }

                    if (m.size > room) { // like a big file: the watermark stays before it until there's room
                        full = true; blocked = true
                        BackupStatus.update { it.copy(total = it.total - 1, bytesTotal = it.bytesTotal - m.size.coerceAtLeast(0)) }
                        continue
                    }
                    val label = "${done + 1} · ${m.name}"
                    BackupStatus.update { it.copy(file = m.name, filePct = if (m.size > 0) 0 else -1) }
                    // The home address was checked on one network for one session. Left home mid-pass
                    // (another network may answer to that name) or logged in again: check again first.
                    if (p.session != session || (!base.startsWith("https://") && cm.activeNetwork != net)) {
                        session = p.session; net = cm.activeNetwork
                        base = Api.pickBase(p, session)
                    }
                    try {
                        val res = openOriginal(ctx, m.uri).use { input ->
                            Api.upload(base, session, m.name, m.takenMs, input, { isStopped }) { sent ->
                                val sentOfSize = if (m.size > 0) minOf(sent, m.size) else 0L
                                BackupStatus.update { it.copy(bytesDone = bytesDone + sentOfSize,
                                    filePct = if (m.size > 0) (sentOfSize * 100 / m.size).toInt() else -1) }
                                val now = System.currentTimeMillis()
                                if (now - lastNotify > 700) {
                                    lastNotify = now
                                    val pct = if (m.size > 0) " ${minOf(100, sent * 100 / m.size)}%" else ""
                                    p.lastStatus = "Uploading $label$pct"
                                    val passTotal = BackupStatus.live()?.total ?: (done + total)
                                    if (Backup.canNotify(ctx)) ctx.getSystemService(NotificationManager::class.java)
                                        .notify(Backup.NOTIF_ID, Backup.progressNotification(ctx, "$label$pct", done, passTotal))
                                }
                            }
                        }
                        when (res) {
                            Api.UploadResult.Stored -> {
                                stored++; p.uploadedTotal = p.uploadedTotal + 1
                                if (room != Long.MAX_VALUE) room -= m.size.coerceAtLeast(0)
                            }
                            Api.UploadResult.Duplicate -> dups++
                            is Api.UploadResult.Rejected -> skipped++
                        }
                    } catch (e: Api.StoppedException) {
                        return
                    } catch (e: Api.AuthException) {
                        // Logged out elsewhere, password reset or person removed. (If someone has logged in
                        // again meanwhile, keep that session: the next pass uploads with it.)
                        if (p.sessionEnded(session)) {
                            p.lastStatus = "Stopped: please log in again"
                            authNotification(ctx, p.name)
                        }
                        return
                    } catch (e: Api.FullException) {
                        // The limit changed meanwhile, or another device used the space: ask what's left.
                        full = true; blocked = true
                        room = minOf(room, Api.room(base, session))
                        BackupStatus.update { it.copy(total = it.total - 1, bytesTotal = it.bytesTotal - m.size.coerceAtLeast(0)) }
                        continue
                    } catch (e: FileNotFoundException) {
                        // Really deleted, or just temporarily unavailable (SD card removed mid-pass)?
                        if (Backup.exists(ctx, m.uri)) throw Transient("Couldn't read ${m.name}. Will retry.", BackupStatus.PROBLEM_READ)
                        skipped++
                    } catch (e: IOException) {
                        // When the space is already used up the Pi answers before reading the file, which
                        // can look like a broken connection here. Ask before calling it a network problem.
                        val left = Api.room(base, session)
                        if (left != Long.MAX_VALUE && m.size > left) {
                            full = true; blocked = true; room = left
                            BackupStatus.update { it.copy(total = it.total - 1, bytesTotal = it.bytesTotal - m.size.coerceAtLeast(0)) }
                            continue
                        }
                        summarize(p, stored, dups, skipped, bigWaiting)
                        throw Transient("Couldn't reach PiFoto (${e.message?.take(60)}). Will retry.", BackupStatus.PROBLEM_NET)
                    }
                    advance(m)
                    done++; uploadedThisRound++
                    bytesDone += m.size.coerceAtLeast(0)
                    BackupStatus.update { it.copy(done = done, bytesDone = bytesDone, file = "", filePct = -1) }
                }
                if (!blocked) p.setDoneAbove(volume, emptySet()) // everything is at/below the watermark now
            }
            if (uploadedThisRound == 0) break
        }
        summarize(p, stored, dups, skipped, bigWaiting)
        p.lastSummary = p.lastStatus
        if (full) { // no 15-minute retry: nothing changes until there's room; the hourly run checks again
            p.lastStatus = fullText(stored)
            p.problem = BackupStatus.PROBLEM_FULL
        } else p.problem = "" // a complete pass: nothing is waiting for a retry
    }

    private fun fullText(stored: Int) = (if (stored > 0) "$stored backed up. The rest doesn't fit your storage on the Pi:" else
        "Your storage on the Pi is full:") + " it waits until an admin gives you more space."

    /** uploadedTotal is counted per file as it is stored (so stopped passes count too). */
    private fun summarize(p: Prefs, stored: Int, dups: Int, skipped: Int, bigWaiting: Boolean) {
        val parts = mutableListOf<String>()
        if (stored + dups + skipped == 0) parts += "Up to date" else parts += "$stored backed up"
        if (dups > 0) parts += "$dups already there"
        if (skipped > 0) parts += "$skipped skipped"
        if (bigWaiting) parts += "large video waiting — open PiFoto to finish it"
        p.lastStatus = parts.joinToString(" · ")
    }

    /** Original bytes incl. GPS metadata when ACCESS_MEDIA_LOCATION is granted. */
    private fun openOriginal(ctx: Context, uri: Uri) =
        (if (ctx.checkSelfPermission(Manifest.permission.ACCESS_MEDIA_LOCATION) == PackageManager.PERMISSION_GRANTED) {
            try { ctx.contentResolver.openInputStream(MediaStore.setRequireOriginal(uri)) }
            catch (_: SecurityException) { null } catch (_: UnsupportedOperationException) { null }
        } else null)
            ?: ctx.contentResolver.openInputStream(uri)
            ?: throw FileNotFoundException(uri.toString())

    private fun authNotification(ctx: Context, name: String) {
        if (!Backup.canNotify(ctx)) return
        Backup.ensureChannel(ctx)
        val n = Notification.Builder(ctx, Backup.CHANNEL)
            .setSmallIcon(R.drawable.ic_notif)
            .setContentTitle("PiFoto backup paused")
            .setContentText(if (name.isNotEmpty()) "Log in again as $name to continue backing up."
                else "Please log in again to continue backing up.")
            .setContentIntent(Backup.openSettingsIntent(ctx))
            .setAutoCancel(true)
            .build()
        ctx.getSystemService(NotificationManager::class.java).notify(Backup.NOTIF_AUTH_ID, n)
    }
}
