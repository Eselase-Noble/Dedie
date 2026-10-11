package org.dedie.desktop;

/**
 * Entry point of the desktop application.
 *
 * <p>The main class deliberately does not extend {@code Application}. When it
 * does, the Java launcher insists on finding JavaFX on the module path, which
 * breaks a plain {@code java -jar}. Starting from an ordinary class lets
 * JavaFX load from the class path like any other library.
 */
public final class Launcher {

    private Launcher() {
    }

    public static void main(String[] args) {
        DedieApp.launch(DedieApp.class, args);
    }
}
