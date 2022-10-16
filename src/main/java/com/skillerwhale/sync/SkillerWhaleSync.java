package com.skillerwhale.sync;

import java.io.*;
import java.util.*;
import java.util.regex.*;
import java.util.logging.*;
import java.util.concurrent.TimeUnit;
import java.time.Duration;

import java.nio.*;
import java.nio.charset.*;
import java.nio.file.*;
import java.nio.file.attribute.*;
import static java.nio.file.LinkOption.*;

import java.net.http.*;
import java.net.URI;

public class SkillerWhaleSync {
    /* Logging */
    {
        //  String.format(format, date, source, logger, level, message, thrown);
        System.setProperty("java.util.logging.SimpleFormatter.format", "%1$tF %1$tT %4$.1s %5$s%6$s%n");
    }
    private static final Logger LOG = Logger.getLogger( SkillerWhaleSync.class.getName() );

    /* Performance tuning */
    public static final int MAX_UPLOAD_BYTES = 10_000_000;
    public static final int SEND_AFTER_MILLIS = 100;
    public static final int WAIT_POLL_MILLIS = 1000;
    public static final int PING_EVERY_MILLIS = 2000; // multiple of WAIT_POLL_MILLIS
    public static final int PING_WARNING_MILLIS = 5000;

    /* Parsed configuration */
    private final String serverUrl;
    private final Path attendanceIdFile;
    private final Path base;
    private final String[] ignoreDirs;
    private final String[] watchedExts;

    /* Runtime */
    private final WatchService watcher;
    interface PathEventFired { void event(WatchEvent<Path> e) throws IOException; }
    private final Map<WatchKey,List<PathEventFired>> watchKeys;
    private final Map<Path,Long> filesToPostTimes;
    private String attendanceId;
    private boolean attendanceIdValid;

    private void postJSON(String uri, String data) throws IOException, InterruptedException {
        // Don't forget `java -Djdk.httpclient.HttpClient.log=headers,requests` for debugging
        HttpClient client = HttpClient.newBuilder().
            connectTimeout(Duration.ofSeconds(2)).
            build();
        HttpRequest request = HttpRequest.newBuilder().
            uri(URI.create(uri)).
            POST(HttpRequest.BodyPublishers.ofString(data)).
            timeout(Duration.ofSeconds(4)).
            header("Content-Type", "application/json").
            build();
        HttpResponse<?> response = client.send(request, HttpResponse.BodyHandlers.ofString());

        if (response.statusCode() >= 200 && response.statusCode() < 300) {
            return;
        }

        if (response.statusCode() >= 400 && response.statusCode() < 500) {
            attendanceIdValid = false;
            if (attendanceIdFile == null) {
                throw new IOException("No valid attendance_id, set ATTENDANCE_ID_FILE or a correct ATTENDANCE_ID");
            }
        }

        LOG.log(Level.WARNING, response.body().toString());
        // Ignore 500 errors
    }

    private boolean postToTrainEndpoint(String endpoint, String data) throws IOException, InterruptedException {
        if (!attendanceIdValid)
            return false;
        postJSON(serverUrl+"attendances/"+attendanceId+"/"+endpoint, data);
        return true;
    }

    /* Turn an unknown source code file read off the disc into a String */
    public static String bytesToString(byte[] data) {
        String tryEncoding = "UTF-8";

        if (data.length > 3 && data[0] == 0xef && data[1] == 0xbb && data[2] == 0xbf) {
            // Byte-order mark (Windows Note) header, claims to be UTF-8
            data = Arrays.copyOfRange(data, 3, data.length);
        } else {
            // Check for Python magic encoding line
            String asciiHeader = StandardCharsets.US_ASCII.decode(ByteBuffer.wrap(data, 0, data.length > 1000 ? 1000 : data.length)).toString();
            Matcher m = Pattern.compile("^[ \t\f]*#.*?coding[:=][ \t]*([-_.a-zA-Z0-9]+)", Pattern.MULTILINE).matcher(asciiHeader);
            if (m.matches()) {
                tryEncoding = m.group(1);
            }
        }

        for (String tryCharset : new String[]{tryEncoding, "ISO-8859-1"}) {
            CharBuffer     out     = CharBuffer.allocate(data.length);
            CharsetDecoder decoder = Charset.forName(tryCharset).newDecoder();
            // Use 3-argument decode() which will stop on a decoding error rather than bodge
            if (decoder.decode(ByteBuffer.wrap(data), out, false) == CoderResult.UNDERFLOW) {
                out.flip();
                return out.toString();
            }
        }

        return null;
    }

    public static String jsonStringQuote(String string) {
        StringBuilder sb = new StringBuilder("\"");
        for (char c : string.toCharArray())
            sb.append(switch (c) {
                case '\\', '"', '/' -> "\\"+c;
                case '\b' -> "\\b";
                case '\t' -> "\\t";
                case '\n' -> "\\n";
                case '\f' -> "\\f";
                case '\r' -> "\\r";
                default -> c < ' ' ? String.format("\\u%04x", (int) c) : c;
            });
        return sb.append('"').toString();
    }

    private boolean postFileSnapshot(Path path, byte[] contents) throws IOException, InterruptedException {
        String contentsAsString = bytesToString(contents);
        if (contentsAsString == null) {
            LOG.log(Level.WARNING, "Couldn't decode "+path+" as string, will not post");
            return false;
        }
        return postToTrainEndpoint(
            "file_snapshots",
            "{"+
            "\"relative_path\": "+ jsonStringQuote(base.relativize(path).toString()) +
            ","+
            "\"contents\": "+jsonStringQuote(contentsAsString)
            +" }"
        );
    }

    private boolean postPing() throws IOException, InterruptedException {
        return postToTrainEndpoint("pings", "");
    }

    private void readAttendanceId() {
        try {
            if (Files.exists(attendanceIdFile, LinkOption.NOFOLLOW_LINKS) && Files.size(attendanceIdFile) < 100) {
                String newAttendanceId = Files.readString(attendanceIdFile).replaceAll("\\s","");
                if (!newAttendanceId.equals(attendanceId)) {
                    attendanceId = newAttendanceId;
                    attendanceIdValid = true;
                    postPing();
                    if (attendanceIdValid == true) {
                        LOG.log(Level.INFO, "valid   {0}", attendanceId);
                    } else {
                        LOG.log(Level.INFO, "invalid {0}", attendanceId);
                    }
                }
            }
        }
        catch (IOException | InterruptedException e) {
            LOG.log(Level.WARNING, "", e);
        }
    }

    void registerDir(Path p, PathEventFired handler) throws IOException {
        WatchKey k = p.register(watcher,
            StandardWatchEventKinds.ENTRY_CREATE,
            StandardWatchEventKinds.ENTRY_DELETE,
            StandardWatchEventKinds.ENTRY_MODIFY
        );
        List<PathEventFired> handlers = watchKeys.getOrDefault(k, new ArrayList<PathEventFired>());
        handlers.add(handler);
        watchKeys.put(k, handlers);
    }

    void registerAttendanceIdWatcher() throws IOException {
        registerDir(attendanceIdFile.getParent(), (WatchEvent<Path> event) -> {
            if (event.kind() == StandardWatchEventKinds.ENTRY_MODIFY && event.context().endsWith(attendanceIdFile.getFileName())) {
                readAttendanceId();
            }
        });
    }

    private void registerDirectoryWatcher(final Path start) throws IOException {
        LOG.log(Level.INFO, "watching file tree "+start.toString());
        Files.walkFileTree(start, new SimpleFileVisitor<Path>() {
            @Override
            public FileVisitResult preVisitDirectory(Path dir, BasicFileAttributes attrs)
                throws IOException
            {
                registerDir(dir, (WatchEvent<Path> event) -> {
                    Path child = dir.resolve(event.context().toString()).toAbsolutePath();

                    boolean matchExt = Arrays.stream(watchedExts).
                        anyMatch(s -> child.toString().endsWith(s));
                    boolean matchIgnore = Arrays.stream(ignoreDirs).
                        anyMatch(s -> child.toString().contains(s+"/"));

                    LOG.log(Level.FINEST, "child={0}, matchExt="+matchExt+", matchIgnore="+matchIgnore, child.toString());

                    if (event.kind() == StandardWatchEventKinds.ENTRY_CREATE && Files.isDirectory(child, NOFOLLOW_LINKS)) {
                        registerDirectoryWatcher(child);
                        return;
                    }

                    if ((event.kind() == StandardWatchEventKinds.ENTRY_CREATE || event.kind() == StandardWatchEventKinds.ENTRY_MODIFY) &&
                        Files.isRegularFile(child, LinkOption.NOFOLLOW_LINKS) &&
                        child.startsWith(base) &&
                        matchExt && !matchIgnore) {

                            filesToPostTimes.put(child, System.currentTimeMillis() + SEND_AFTER_MILLIS);

                    }
                });
                return FileVisitResult.CONTINUE;
            }
        });
    }

    void readAndPostFileSnapshot(Path file) throws InterruptedException, IOException {
        try (FileInputStream in = new FileInputStream(file.toString())) {
            var len = in.available();
            if (len < MAX_UPLOAD_BYTES) {
                var buffer = new byte[len];
                if (in.read(buffer) != len) {
                    throw new IOException("Couldn't read all of "+file+" in one go");
                }
                if (postFileSnapshot(file, buffer)) {
                    LOG.log(Level.INFO,    "upload {0}", file);
                } else {
                    LOG.log(Level.WARNING, "ignore {0}", file);
                }
            } else {
                    LOG.log(Level.WARNING, "too large {0}", file);
            }
        }
    }

    long nextFlushAt() {
        return filesToPostTimes.values().stream().reduce(Long.MAX_VALUE, (soonest,t) -> t < soonest ? t : soonest);
    }

    void flushOverdueFileSnapshots() throws IOException, InterruptedException {
        for (var e : filesToPostTimes.entrySet()) {
            Path path = e.getKey();
            long updateTime = e.getValue();
            if (System.currentTimeMillis() > updateTime) {
                readAndPostFileSnapshot(path);
                filesToPostTimes.remove(path);
            }
        }
    }

    /**
     * Process all events for keys queued to the watcher
     */
    boolean waitForFileUpdates(long until) throws InterruptedException, IOException {
        WatchKey key = watcher.poll(until - System.currentTimeMillis(), TimeUnit.MILLISECONDS);
        if (key == null) {
            return false;
        }
        List<PathEventFired> handlers = watchKeys.get(key);
        assert(handlers != null);

        for (WatchEvent<?> event : key.pollEvents()) {
            for (PathEventFired handler : handlers) {
                try {
                    @SuppressWarnings("unchecked")
                    var eventPath = (WatchEvent<Path>) event;
                    handler.event(eventPath);
                }
                catch (ClassCastException x) {
                    if (event.kind() == StandardWatchEventKinds.OVERFLOW) {
                        LOG.log(Level.WARNING, "overflow, might have missed something");
                    }
                }
            }
        }

        if (!key.reset()) {
            watchKeys.remove(key);
        }

        return true;
    }

    class ConfigError extends Error {
        ConfigError(String s) { super(s); }
    }

    SkillerWhaleSync(String attendanceId, Path attendanceIdFile, String serverUrl, Path base, String[] watchedExts, String[] ignoreDirs) throws IOException {
        this.attendanceId = attendanceId;
        this.attendanceIdFile = attendanceIdFile;
        this.serverUrl = serverUrl;
        this.base = base;
        this.watchedExts = watchedExts;
        this.ignoreDirs = ignoreDirs;

        this.attendanceIdValid = this.attendanceId != null;
        this.watcher = FileSystems.getDefault().newWatchService();
        this.watchKeys = new HashMap<WatchKey,List<PathEventFired>>();
        this.filesToPostTimes = new HashMap<Path,Long>();

        readAttendanceId();

        if (attendanceIdFile != null) {
            attendanceIdFile = attendanceIdFile.toAbsolutePath();
            registerAttendanceIdWatcher();
        }

        if (!attendanceIdValid) {
            if (attendanceIdFile == null) {
                throw new ConfigError("Can't start without either ATTENDANCE_ID or an ATTENDANCE_ID_FILE");
            }
            LOG.log(Level.INFO, "Set attendance_id in '"+attendanceIdFile+"' file to start synchronisation", attendanceId);
        }

        if (watchedExts.length == 0) {
            throw new ConfigError("WATCHED_EXTS is empty");
        }

        if (ignoreDirs.length == 0) {
            LOG.log(Level.WARNING, "IGNORE_DIRS is empty");
        }

        registerDirectoryWatcher(base);
    }

    public static String[] getenvAndSplit(Map<String,String> e, String name) {
        String v = e.getOrDefault(name, null);
        if (v == null) {
            return new String[0];
        }
        // deal with simple JSON array
        return v.replaceAll("[\\[\\]\"]","").split(" +");
    }

    public static SkillerWhaleSync createFromEnvironment(Map<String,String> e) throws IOException {
        return new SkillerWhaleSync(
            e.getOrDefault("ATTENDANCE_ID", null),
            Paths.get(e.getOrDefault("ATTENDANCE_ID_FILE", "attendance_id")),
            e.getOrDefault("SERVER_URL", "https://train.skillerwhale.com/"),
            Paths.get(e.getOrDefault("WATCHER_BASE_PATH", ".")).normalize().toAbsolutePath(),
            getenvAndSplit(e, "WATCHED_EXTS"),
            getenvAndSplit(e, "IGNORE_DIRS")
        );
    }

    public static void logo() {
        System.out.println("     skillerwhale.com file synchronisation      ");
        try (InputStream in = SkillerWhaleSync.class.getResourceAsStream("/logo")) {
            System.out.write(in.readAllBytes());
        }
        catch (IOException i) { /* no logo */ }
    }

    public static void main(String[] args) throws IOException, InterruptedException {
        SkillerWhaleSync sync = createFromEnvironment(System.getenv());
        int slowPingWarnings = 0;
        long nextPing = 0;
        logo();

        while(true) {
            if (nextPing < System.currentTimeMillis()) {
                if (nextPing != 0 && System.currentTimeMillis() - nextPing > PING_WARNING_MILLIS) {
                    LOG.warning("Slow ping warnings: "+(++slowPingWarnings));
                }
                sync.postPing();
                nextPing = System.currentTimeMillis() + PING_EVERY_MILLIS;
            }

            sync.waitForFileUpdates(
                Math.min(System.currentTimeMillis() + WAIT_POLL_MILLIS,
                Math.min(nextPing, sync.nextFlushAt()))
            );
            sync.flushOverdueFileSnapshots();
        }
    }
}
