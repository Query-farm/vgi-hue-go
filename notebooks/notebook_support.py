"""Presentation helpers; analytical transformations remain visible SQL."""

from pathlib import Path
from typing import Any

import matplotlib.pyplot as plt
import seaborn as sns


class HaybarnSession:
    """Keep JupySQL statements on one native session so explicit transactions stay in one session."""

    def __init__(self, connection: Any) -> None:
        """Wrap the native connection without changing SQL or its results."""
        self._connection = connection

    def cursor(self) -> Any:
        """Reuse the native session instead of creating a new transaction scope."""
        return self._connection

    def commit(self) -> None:
        """Delegate an explicit DBAPI commit operation."""
        self._connection.commit()

    def close(self) -> None:
        """Close the native connection and its worker processes."""
        self._connection.close()


def theme():
    """Use consistent readable chart defaults."""
    sns.set_theme(style="whitegrid", context="notebook", font_scale=1.08)
    plt.rcParams.update(
        {
            "figure.dpi": 130,
            "savefig.dpi": 150,
            "text.parse_math": False,
            "axes.spines.top": False,
            "axes.spines.right": False,
        }
    )


def finish(fig, name, title, subtitle):
    """Reserve title and source space, export a local review chart, and display."""
    fig.suptitle(title, x=0.10, y=0.98, ha="left", fontsize=17, fontweight="bold", color="#243e32")
    fig.text(0.10, 0.922, subtitle, ha="left", fontsize=11, color="#526b5b")
    fig.text(
        0.10,
        0.025,
        "Source: repository synthetic Hue bridge · disposable local fixture · no real devices\n"
        "Real vgi-hue-go through Haybarn / VGI · brightness settings are not power or energy measurements",
        fontsize=9,
        color="#526b5b",
    )
    fig.tight_layout(rect=(0.02, 0.105, 0.98, 0.85))
    folder = Path("validation/charts")
    folder.mkdir(parents=True, exist_ok=True)
    fig.savefig(folder / f"{name}.png", bbox_inches="tight")
    plt.show()
