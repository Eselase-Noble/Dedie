package org.dedie.desktop;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStreamReader;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.TimeUnit;

/**
 * The Dedie engine, run as a child process.
 *
 * <p>The desktop application has no logic of its own. It starts the same
 * {@code dedie-server} program that teams run on a server, bound to a free
 * loopback port that nothing outside this machine can reach, and shows its
 * interface. Queries, migrations, backups, access control and auditing
 * therefore behave exactly as they do everywhere else.
 */
final class Engine {

    /** The line the server prints once it is ready to accept connections. */
    private static final String URL_PREFIX = "DEDIE_URL=";

    private static final String BINARY = isWindows() ? "dedie-server.exe" : "dedie-server";

    private Process process;

    /**
     * Starts the engine and completes with the address of its interface.
     * The future fails if the program cannot be found or exits early.
     */
    CompletableFuture<String> start() {
        CompletableFuture<String> url = new CompletableFuture<>();
        Path binary;
        try {
            binary = locate();
            // Standard error carries the server's log; let it through to
            // whatever started the desktop application.
            process = new ProcessBuilder(binary.toString(), "--addr", "127.0.0.1:0")
                    .redirectError(ProcessBuilder.Redirect.INHERIT)
                    .start();
        } catch (IOException e) {
            url.completeExceptionally(e);
            return url;
        }

        Thread reader = new Thread(() -> {
            try (BufferedReader out = new BufferedReader(
                    new InputStreamReader(process.getInputStream(), StandardCharsets.UTF_8))) {
                String line;
                while ((line = out.readLine()) != null) {
                    if (line.startsWith(URL_PREFIX)) {
                        url.complete(line.substring(URL_PREFIX.length()).trim());
                    }
                }
            } catch (IOException ignored) {
                // The stream closes when the process ends; handled below.
            }
            url.completeExceptionally(new IOException(
                    "The Dedie engine stopped before it was ready. Run " + binary + " in a terminal to see why."));
        }, "dedie-engine-output");
        reader.setDaemon(true);
        reader.start();
        return url;
    }

    /**
     * Stops the engine and waits for it to finish. The server hands running
     * migrations back to its queue on a normal stop, so they resume the next
     * time the application starts.
     */
    void stop() {
        if (process == null || !process.isAlive()) {
            return;
        }
        process.destroy();
        try {
            if (!process.waitFor(20, TimeUnit.SECONDS)) {
                process.destroyForcibly();
            }
        } catch (InterruptedException e) {
            process.destroyForcibly();
            Thread.currentThread().interrupt();
        }
    }

    /**
     * Finds the server program. In order: the {@code dedie.server} system
     * property, the {@code DEDIE_SERVER} environment variable, beside the
     * application's jar, the project's {@code bin} directory when run from
     * the source tree, and finally the {@code PATH}.
     */
    private static Path locate() throws IOException {
        List<Path> candidates = new ArrayList<>();
        addIfSet(candidates, System.getProperty("dedie.server"));
        addIfSet(candidates, System.getenv("DEDIE_SERVER"));

        Path home = applicationDirectory();
        if (home != null) {
            candidates.add(home.resolve(BINARY));
            candidates.add(home.resolve("bin").resolve(BINARY));
        }
        Path cwd = Path.of("").toAbsolutePath();
        for (Path dir = cwd; dir != null; dir = dir.getParent()) {
            candidates.add(dir.resolve("bin").resolve(BINARY));
        }
        String path = System.getenv("PATH");
        if (path != null) {
            for (String dir : path.split(java.io.File.pathSeparator)) {
                if (!dir.isBlank()) {
                    candidates.add(Path.of(dir).resolve(BINARY));
                }
            }
        }
        for (Path candidate : candidates) {
            if (Files.isRegularFile(candidate) && Files.isExecutable(candidate)) {
                return candidate;
            }
        }
        throw new IOException("Could not find " + BINARY + ". Put it next to the application, "
                + "add it to your PATH, or set the DEDIE_SERVER environment variable to its location.");
    }

    private static void addIfSet(List<Path> candidates, String value) {
        if (value != null && !value.isBlank()) {
            candidates.add(Path.of(value));
        }
    }

    /** The directory holding the application's jar, or null when unknown. */
    private static Path applicationDirectory() {
        try {
            Path location = Path.of(Engine.class.getProtectionDomain().getCodeSource().getLocation().toURI());
            return Files.isDirectory(location) ? location : location.getParent();
        } catch (Exception e) {
            return null;
        }
    }

    private static boolean isWindows() {
        return System.getProperty("os.name", "").toLowerCase().contains("win");
    }
}
