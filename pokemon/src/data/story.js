/* All player-facing narrative text. Editing the story = editing this file. */
export const STORY = {
  intro: [
    "You wake up in your room in Willow Town.",
    "Today you receive your first Shapemon!",
    "Visit Prof. Cedar's lab, then head north to",
    "challenge the Fernwood Gym. The Heal Center",
    "and Rocky Cave are open to explore, too.",
  ],
  profGive: [
    "Prof. Cedar: There you are!",
    "As arranged, your partner is the Fire-type,",
    "Emberling. Take good care of it!",
    "Emberling joined your party!",
    "Now earn the Leaf Badge at Fernwood Gym, north of town.",
  ],
  profDone: ["Prof. Cedar: Emberling's flame burns bright. Go get that badge!"],
  nurse: ["Nurse: Welcome to the Heal Center!", "Your Shapemon are fully healed."],
  gymNoStarter: ["Leader Fern: No Shapemon? Come back when you're ready."],
  gymIntro: [
    "Leader Fern: Welcome to Fernwood Gym!",
    "My Grass-types have deep roots. Show me your fire!",
  ],
  gymDone: [
    "Leader Fern: The Leaf Badge suits you. Well fought!",
    "The gate north of town is open now. Beyond it lies",
    "Tidewater Town and its Water gym. Good luck!",
  ],
  gym2NoStarter: ["Leader Marina: You wandered in without a team? Off you go."],
  gym2Intro: [
    "Leader Marina: So you cleared Fernwood. Impressive.",
    "But my Water-types will douse that flame. Come on!",
  ],
  gym2Done: [
    "Leader Marina: Two badges! You're the real deal.",
    "The final gate to the north is yours to open.",
  ],
  gateLocked0: [
    "A gate blocks the way north.",
    "You need the Leaf Badge to pass. Beat Fernwood Gym first!",
  ],
  gateLocked1: [
    "The final gate is sealed.",
    "Earn the Tidewater Badge from Leader Marina first!",
  ],

  // Walk-up NPC chatter, keyed by the npc's `dialog` id (see data/maps.js).
  npc: {
    kid: [
      "Kid: The tall grass is full of wild Shapemon!",
      "Walk through it and you might run into one.",
    ],
    oldman: [
      "Old Man: Fire beats Grass, Grass beats Water,",
      "and Water beats Fire. Type match-ups win battles!",
    ],
    hiker: [
      "Hiker: Rocky Cave to the north-east is crawling",
      "with tough Shapemon. Heal up before you go in!",
    ],
    swimmer: [
      "Swimmer: Catch a Grass-type in the tall grass here —",
      "it'll make short work of Marina's Water gym!",
    ],
  },

  // Trainer battles, keyed by the npc's `dialog` id (see data/maps.js).
  trainers: {
    rick: {
      intro: ["Camper Rick: Hey, you look tough!", "Let's battle!"],
      win: ["Camper Rick: Wow, your team is strong!"],
    },
    mia: {
      intro: ["Scout Mia: A trainer never backs down.", "Here I come!"],
      win: ["Scout Mia: You've really bonded with your Shapemon."],
    },
    kai: {
      intro: ["Sailor Kai: These waters are my turf!", "Prove yourself!"],
      win: ["Sailor Kai: You've got the current on your side."],
    },
  },

  shopWelcome: ["Clerk: Welcome to the Mart! What can I get you?"],
};

// Rolling credits shown after clearing the final gym.
export const CREDITS = [
  "THANK YOU FOR PLAYING!", "", "You earned the Leaf and Tidewater Badges",
  "and became a Shapemon Champion!", "", "Shapemon — Ember Quest",
  "An original Gen-1/3-style demo", "",
  "Battle engine .. authentic FireRed math",
  "Creatures ...... Emberling line, Wormling line,",
  "                 Nibbit, Cavvit, Dribblet line,",
  "                 Thornbud", "",
  "Design & Code .. you + Claude", "", "~ The End ~",
];
